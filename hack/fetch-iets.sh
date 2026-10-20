#!/usr/bin/env bash
# Downloads the prebuilt iets static library for one Go platform from this
# repository's releases (.github/workflows/iets-lib.yml publishes them under
# the tag iets-lib-<revision>) and verifies its checksum.
#
#   hack/fetch-iets.sh [goos/goarch]     # default: the host
#
# OAR_IETS_LIB_URL names another base URL that serves
# libiets_ffi-<goos>_<goarch>.a and libiets_ffi-<goos>_<goarch>.a.sha256.
# Exits non-zero when nothing could be fetched, so a caller can build from
# source instead (hack/build-iets.sh).
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)

platform=${1:-"$(go env GOOS)/$(go env GOARCH)"}
goos=${platform%/*}
goarch=${platform#*/}
rev=$(sed -n 's/.*rev = "\([0-9a-f]*\)".*/\1/p' "$ROOT/rust/iets-ffi/Cargo.toml" | head -1)
base=${OAR_IETS_LIB_URL:-"https://github.com/AkaraChen/open-agent-runtime/releases/download/iets-lib-${rev:0:12}"}
name="libiets_ffi-${goos}_${goarch}.a"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "fetching $base/$name"
if ! curl -fsSL --retry 2 -o "$tmp/$name" "$base/$name" || ! curl -fsSL --retry 2 -o "$tmp/$name.sha256" "$base/$name.sha256"; then
  echo "no prebuilt library at $base" >&2
  exit 1
fi
want=$(cut -d' ' -f1 "$tmp/$name.sha256")
if command -v sha256sum >/dev/null; then
  got=$(sha256sum "$tmp/$name" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$tmp/$name" | cut -d' ' -f1)
fi
if [ "$got" != "$want" ]; then
  echo "checksum mismatch for $name: got $got, want $want" >&2
  exit 1
fi
dest="$ROOT/internal/nixeval/iets/lib/${goos}_${goarch}"
mkdir -p "$dest"
mv "$tmp/$name" "$dest/libiets_ffi.a"
printf '%s\n' "$rev" > "$dest/IETS_REV"
printf '%s\n' "$got" > "$dest/SHA256"
echo "installed $dest/libiets_ffi.a (iets $rev)"
