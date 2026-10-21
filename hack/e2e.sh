#!/usr/bin/env bash
# End-to-end acceptance test of the MVP against one backend.
#
#   BACKEND=native|kvm|qemu|vz hack/e2e.sh [extra `oar vm start` flags]
#
# Checks:
#   1. Debian slim + 3 nix packages + 1 custom binary starts
#   2. shuffled component order => identical bundle digest and environment
#   3. warm compose < 1s
#   4. runc start / stop / delete (and reload) work
#   5. the guest is resident: it is booted once and reused
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OAR=${OAR:-$ROOT/bin/oar}
EX=$ROOT/examples/agent-runtime
BACKEND=${BACKEND:-native}
# a private state directory: $XDG_DATA_HOME/oar
export XDG_DATA_HOME=$(mktemp -d) # never the user's own state
OAR_STATE=$XDG_DATA_HOME/oar
case "$(uname -m)" in arm64|aarch64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
ARCH=${GUEST_ARCH:-$ARCH}
PLATFORM=linux/$ARCH
NAME=e2e
WORK=$(mktemp -d)

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
oar() { "$OAR" -f "$EX/oar.yaml" "$@"; }

cleanup() {
  status=$?
  if [ $status -ne 0 ]; then
    echo "---- diagnostics ----"
    for f in "$OAR_STATE"/vms/default/{supervisor.log,console.log,qemu.log}; do
      [ -f "$f" ] && { echo "## $f"; tail -n 60 "$f"; }
    done
  fi
  "$OAR" vm stop >/dev/null 2>&1 || true
  rm -f "$EX"/oar.shuffled-*.yaml
  exit $status
}
trap cleanup EXIT

step "build artifacts for linux/amd64 and linux/arm64"
oar build
oar build --json | jq -e --arg p "$PLATFORM" '.platforms[$p].bundleDigest' >/dev/null
DIGEST=$(oar digest --platform "$PLATFORM")
echo "bundle digest ($PLATFORM): $DIGEST"

step "2. shuffled component order gives the same digest"
python3 -I - "$EX/oar.yaml" "$EX" <<'PY'
import itertools, random, sys
src, outdir = sys.argv[1], sys.argv[2]
lines = open(src).read().split("\n")
start = lines.index("components:") + 1
end = start
while end < len(lines) and lines[end].startswith("  - "):
    end += 1
items = lines[start:end]
perms = list(itertools.permutations(items))
random.seed(42)
chosen = [p for p in perms if list(p) != items]
random.shuffle(chosen)
for i, p in enumerate(chosen[:3]):
    out = lines[:start] + list(p) + lines[end:]
    open(f"{outdir}/oar.shuffled-{i}.yaml", "w").write("\n".join(out))
    print(f"oar.shuffled-{i}.yaml:", ", ".join(x.strip()[2:] for x in p))
PY
for f in "$EX"/oar.shuffled-*.yaml; do
  "$OAR" -f "$f" build --platform "$PLATFORM" >/dev/null
  d=$("$OAR" -f "$f" digest --platform "$PLATFORM")
  [ "$d" = "$DIGEST" ] || fail "digest of $(basename "$f") is $d, expected $DIGEST"
  echo "ok $(basename "$f") -> $d"
done
oar build --platform "$PLATFORM" >/dev/null

step "5. start the resident guest ($BACKEND)"
"$OAR" vm start --backend "$BACKEND" --arch "$ARCH" "$@"
"$OAR" vm status
BOOTED=$("$OAR" vm status --json | jq -r .agent.bootedAt)

step "1. run Debian slim + nodejs_22 + python313 + git + my-agent"
oar run --name "$NAME"
for i in $(seq 1 50); do
  oar logs --name "$NAME" | grep -q "my-agent ready" && break
  sleep 0.2
done
oar logs --name "$NAME"
REPORT=$(oar logs --name "$NAME" | sed -n 's/^my-agent ready //p' | head -1)
[ -n "$REPORT" ] || fail "my-agent did not report"
echo "$REPORT" | jq -e '.node | startswith("v22.")' >/dev/null || fail "node 22 missing"
echo "$REPORT" | jq -e '.python | startswith("Python 3.13")' >/dev/null || fail "python 3.13 missing"
echo "$REPORT" | jq -e '.git | startswith("git version")' >/dev/null || fail "git missing"
echo "$REPORT" | jq -e '.os | test("Debian")' >/dev/null || fail "not a Debian rootfs"
echo "$REPORT" | jq -e --arg a "$ARCH" '.arch == $a' >/dev/null || fail "wrong arch"
oar exec --name "$NAME" -- sh -c 'test -x /opt/my-agent && ! touch /nix/store/.w 2>/dev/null && echo "nix store is read-only, /opt/my-agent present"'
oar exec --name "$NAME" -- node -e 'console.log("node ok", process.version)'
WANT=$(jq '.storePaths | length' "$EX/artifacts/manifests/bundles/${DIGEST#sha256:}.json")
GOT=$(oar exec --name "$NAME" -- sh -c 'ls /nix/store | wc -l' | tr -d ' ')
[ "$GOT" = "$WANT" ] || fail "/nix/store shows $GOT entries, the bundle closure has $WANT"
echo "/nix/store exposes exactly the bundle closure ($GOT store paths)"
oar exec --name "$NAME" -- python3 -c 'import ssl, sqlite3; print("python ok", ssl.OPENSSL_VERSION)'
oar exec --name "$NAME" -- git init -q /tmp/repo
oar exec --name "$NAME" -- cat /tmp/my-agent.json > "$WORK/env-original.json"

step "2b. shuffled config yields the same observable environment"
"$OAR" -f "$EX/oar.shuffled-0.yaml" run --name "${NAME}-shuffled" --no-build
for i in $(seq 1 50); do
  "$OAR" -f "$EX/oar.shuffled-0.yaml" exec --name "${NAME}-shuffled" -- cat /tmp/my-agent.json > "$WORK/env-shuffled.json" 2>/dev/null && break
  sleep 0.2
done
diff <(jq -S . "$WORK/env-original.json") <(jq -S . "$WORK/env-shuffled.json") || fail "environment differs"
SHUF_DIGEST=$("$OAR" ps --json | jq -r --arg n "${NAME}-shuffled" '.containers[] | select(.name==$n) | .bundleDigest')
[ "$SHUF_DIGEST" = "$DIGEST" ] || fail "running shuffled bundle digest $SHUF_DIGEST != $DIGEST"
echo "identical environment and digest: $DIGEST"
"$OAR" stop --name "${NAME}-shuffled" --rm

step "3. warm compose < 1s"
MAX=0
for i in 1 2 3 4 5; do
  start=$(python3 -I -c 'import time; print(time.time())')
  out=$(oar compose --json)
  end=$(python3 -I -c 'import time; print(time.time())')
  wall=$(python3 -I -c "print(round(($end-$start)*1000, 1))")
  guest=$(echo "$out" | jq -r '.guestDuration / 1e6')
  cached=$(echo "$out" | jq -r .cached)
  echo "compose #$i: wall ${wall}ms (CLI process incl.), guest ${guest}ms, cached=$cached"
  [ "$(echo "$out" | jq -r .bundleDigest)" = "$DIGEST" ] || fail "compose digest mismatch"
  MAX=$(python3 -I -c "print(max($MAX, $wall))")
done
python3 -I -c "import sys; sys.exit(0 if $MAX < 1000 else 1)" || fail "warm compose took ${MAX}ms"
echo "max warm compose wall time: ${MAX}ms"

step "4. reload, stop, delete via runc"
OLD=$("$OAR" ps --json | jq -r --arg n "$NAME" '.containers[] | select(.name==$n) | .id')
oar reload --name "$NAME"
NEW=$("$OAR" ps --json | jq -r --arg n "$NAME" '.containers[] | select(.name==$n) | .id')
[ "$OLD" != "$NEW" ] || fail "reload kept the same instance"
"$OAR" ps --json | jq -e --arg o "$OLD" '[.containers[] | select(.id==$o)] | length == 0' >/dev/null || fail "old instance not destroyed"
[ "$("$OAR" ps --json | jq -r --arg n "$NAME" '.containers[] | select(.name==$n) | .status')" = running ] || fail "new instance not running"
oar stop --name "$NAME"
[ "$("$OAR" ps --json | jq -r --arg n "$NAME" '.containers[] | select(.name==$n) | .status')" = stopped ] || fail "not stopped"
oar logs --name "$NAME" | grep -q "received terminated" || fail "my-agent did not get SIGTERM"
oar rm --name "$NAME"
[ "$("$OAR" ps --json | jq '.containers | length')" = 0 ] || fail "container not deleted"
"$OAR" ps

step "5b. the guest stayed resident"
[ "$("$OAR" vm status --json | jq -r .agent.bootedAt)" = "$BOOTED" ] || fail "guest was restarted"
oar run --name "$NAME" --no-build >/dev/null
oar stop --name "$NAME" --rm >/dev/null
echo "guest booted at $BOOTED served every request"

step "PASS ($BACKEND, $PLATFORM)"
