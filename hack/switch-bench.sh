#!/usr/bin/env bash
# Measures switching a running service between two pre-compiled runtime
# configurations, as seen by a client polling it every 5ms.
#
#   BACKEND=native|kvm|qemu|vz hack/switch-bench.sh [switches] [extra vm start flags]
#
# Config A: Debian + nodejs_22 + python313 + git + my-agent (VARIANT=a)
# Config B: same + nix:jq, different env    (VARIANT=b)
# Both are built and warmed first; every switch then uses --bundle <digest>,
# i.e. no config, no build tools, no network. A prober container (python +
# an inline probe script) shares the guest network and records failed
# requests and the largest gap between successful responses.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OAR=${OAR:-$ROOT/bin/oar}
EX=$ROOT/examples/agent-runtime
BACKEND=${BACKEND:-native}
N=${1:-6}
shift || true
export OAR_HOME=${OAR_HOME:-$(mktemp -d)}
case "$(uname -m)" in arm64|aarch64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
ARCH=${GUEST_ARCH:-$ARCH}
PLATFORM=linux/$ARCH
PORT=18080
GEN=()

oar() { "$OAR" --artifacts "$EX/artifacts" "$@"; }
cleanup() {
  status=$?
  [ $status -ne 0 ] && for f in "$OAR_HOME"/vms/default/{supervisor.log,console.log}; do [ -f "$f" ] && tail -n 30 "$f"; done
  "$OAR" vm stop >/dev/null 2>&1 || true
  rm -f "${GEN[@]}"
  exit $status
}
trap cleanup EXIT

# variant <file> <letter> <reuseport 0|1> <extra component or "">
variant() {
  local f=$EX/$1; GEN+=("$f")
  awk -v extra="$4" '{print} /^  - binary:my-agent$/ && extra != "" {print "  - " extra}' "$EX/oar.yaml" |
    sed '/^process:/,$d' > "$f"
  cat >> "$f" <<EOF
process:
  command: ["/opt/my-agent"]
  env:
    VARIANT: "$2"
    MY_AGENT_LISTEN: "127.0.0.1:$PORT"
    MY_AGENT_REUSEPORT: "$3"
  # log-based: with SO_REUSEPORT a tcp check would be answered by the old instance
  ready:
    log: "my-agent: serving on"
    timeout: 10s
EOF
}
variant oar.switch-a-reuse.yaml a 1 ""
variant oar.switch-b-reuse.yaml b 1 nix:jq
variant oar.switch-a-plain.yaml a 0 ""
variant oar.switch-b-plain.yaml b 0 nix:jq
# a broken release: never becomes ready
variant oar.switch-broken.yaml broken 0 ""
sed -i 's/log: "my-agent: serving on"/log: "this line is never printed"/; s/timeout: 10s/timeout: 2s/' "$EX/oar.switch-broken.yaml"

PROBE=$EX/oar.switch-probe.yaml; GEN+=("$PROBE")
cat > "$PROBE" <<EOF
runtime:
  base: debian:trixie-slim
components: [nix:python313, file:probe]
sources:
  probe:
    target: /opt/probe.py
    content: |
      import json, signal, time, urllib.request
      url, stop = "http://127.0.0.1:$PORT/", False
      def term(*_):
          global stop
          stop = True
      signal.signal(signal.SIGTERM, term)
      ok = fail = 0
      last_ok = prev = None
      max_gap = 0.0
      switches = []
      t0 = time.time()
      while not stop:
          t = time.time()
          try:
              with urllib.request.urlopen(url, timeout=0.5) as r:
                  v = r.headers.get("X-Variant"); r.read()
              ok += 1
              if last_ok is not None and prev is not None:
                  max_gap = max(max_gap, t - last_ok)
              last_ok = t
              if v != prev:
                  switches.append(v); prev = v
                  print("probe: now serving", v, flush=True)
          except Exception:
              fail += 1
          time.sleep(0.005)
      print("probe-summary " + json.dumps({"ok": ok, "failed": fail, "maxGapMs": round(max_gap * 1000, 1),
            "switches": len(switches) - 1, "seconds": round(time.time() - t0, 1)}), flush=True)
process:
  command: ["python3", "-u", "/opt/probe.py"]
EOF

echo "==> compile all configurations for $PLATFORM"
for f in oar.switch-{a,b}-{reuse,plain}.yaml oar.switch-broken.yaml oar.switch-probe.yaml; do
  "$OAR" -f "$EX/$f" build --platform "$PLATFORM" >/dev/null
done
digest() { "$OAR" -f "$EX/$1" digest --platform "$PLATFORM"; }
declare -A D
for v in a-reuse b-reuse a-plain b-plain broken probe; do D[$v]=$(digest "oar.switch-$v.yaml"); done
oar bundles

echo "==> start resident guest ($BACKEND) and pre-warm every bundle"
"$OAR" vm start --backend "$BACKEND" --arch "$ARCH" "$@" >/dev/null
for v in a-reuse b-reuse a-plain b-plain broken probe; do oar --bundle "${D[$v]}" warm >/dev/null; done
oar --bundle "${D[probe]}" compose >/dev/null

# the probe's summary is printed on SIGTERM; capture it before removing
bench_run() {
  local strategy=$1 kind=$2
  echo
  echo "==> strategy=$strategy ($kind listener): $N switches between ${D[a-$kind]:7:12} and ${D[b-$kind]:7:12}"
  oar --bundle "${D[a-$kind]}" run --name svc >/dev/null
  for _ in $(seq 1 100); do
    oar exec --name svc -- python3 -c "import urllib.request as u; u.urlopen('http://127.0.0.1:$PORT/', timeout=1)" >/dev/null 2>&1 && break
    sleep 0.1
  done
  oar --bundle "${D[probe]}" run --name probe >/dev/null
  sleep 1
  local maxwall=0
  for i in $(seq 1 "$N"); do
    local target=b; [ $((i % 2)) -eq 0 ] && target=a
    local t0 t1 out wall
    t0=$(python3 -I -c 'import time; print(time.time())')
    out=$(oar --json --bundle "${D[$target-$kind]}" reload --name svc --strategy "$strategy")
    t1=$(python3 -I -c 'import time; print(time.time())')
    wall=$(python3 -I -c "print(round(($t1-$t0)*1000))")
    [ "$wall" -gt "$maxwall" ] && maxwall=$wall
    printf "  switch %d -> %s: %5s ms wall (CLI incl.) | guest: total %4s ms, compose %s ms, ready %4s ms, stop old %4s ms, downtime %s ms\n" \
      "$i" "$target" "$wall" \
      "$(echo "$out" | jq '.result.timings.total/1e6 | round')" \
      "$(echo "$out" | jq '.result.timings.compose/1e5 | round / 10')" \
      "$(echo "$out" | jq '(.result.timings.ready // 0)/1e6 | round')" \
      "$(echo "$out" | jq '.result.timings.stopOld/1e6 | round')" \
      "$(echo "$out" | jq '(.result.timings.downtime // 0)/1e5 | round / 10')"
    sleep 0.5
  done
  sleep 0.5
  oar stop --name probe >/dev/null
  local summary
  summary=$(oar logs --name probe | sed -n 's/^probe-summary //p')
  oar rm --name probe >/dev/null
  oar stop --name svc --rm >/dev/null
  echo "  client view: $summary"
  echo "  max switch wall time: ${maxwall} ms"
  echo "$strategy $kind $maxwall $summary" >> "$SUMMARY"
}

SUMMARY=$(mktemp); GEN+=("$SUMMARY")
bench_run overlap reuse
bench_run replace plain

echo
echo "==> rollback: reload to a release that never becomes ready"
oar --bundle "${D[a-plain]}" run --name svc >/dev/null
OLD=$("$OAR" ps --json | jq -r '.containers[] | select(.name=="svc") | .id')
if oar --bundle "${D[broken]}" reload --name svc 2>"$EX/.rollback.err"; then
  echo "FAIL: broken release was accepted"; exit 1
fi
GEN+=("$EX/.rollback.err")
sed 's/^/  /' "$EX/.rollback.err"
NOW=$("$OAR" ps --json | jq -r '.containers[] | select(.name=="svc") | "\(.id) \(.status)"')
[ "$NOW" = "$OLD running" ] || { echo "FAIL: expected $OLD running, got $NOW"; exit 1; }
oar exec --name svc -- python3 -c "import urllib.request as u; print('  still serving variant', u.urlopen('http://127.0.0.1:$PORT/').headers['X-Variant'])"
oar stop --name svc --rm >/dev/null

echo
echo "==> summary ($BACKEND, $PLATFORM)"
while read -r strategy kind wall json; do
  printf "  %-8s %-6s max switch %5s ms | requests ok %s, failed %s, max gap between responses %s ms\n" \
    "$strategy" "$kind" "$wall" "$(echo "$json" | jq .ok)" "$(echo "$json" | jq .failed)" "$(echo "$json" | jq .maxGapMs)"
done < "$SUMMARY"
python3 -I - "$SUMMARY" <<'PY'
import json, sys
bad = []
for line in open(sys.argv[1]):
    strategy, kind, wall, js = line.split(" ", 3)
    s = json.loads(js)
    if int(wall) >= 1000:
        bad.append(f"{strategy}: switch took {wall} ms")
    if s["switches"] < 1:
        bad.append(f"{strategy}: prober never saw a switch")
sys.exit("FAIL: " + "; ".join(bad) if bad else 0)
PY
echo "PASS: every switch completed in under 1s"
