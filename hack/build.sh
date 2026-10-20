#!/usr/bin/env bash
# Builds bin/oar for the host, with the embedded Nix evaluator (iets) linked
# in when its static library is available. On macOS the binary is built with
# cgo (for Virtualization.framework) and ad-hoc signed with the
# virtualization entitlement.
#
# The library is looked for in internal/nixeval/iets/lib/<goos>_<goarch>/;
# when missing or stale it is downloaded (hack/fetch-iets.sh) or built from
# source (hack/build-iets.sh, needs cargo). OAR_NO_IETS=1 skips all of that
# and builds the evaluator-less binary, which falls back to nix-instantiate
# or Docker at build time.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
mkdir -p bin

goos=$(go env GOOS)
goarch=$(go env GOARCH)
libdir="internal/nixeval/iets/lib/${goos}_${goarch}"
want=$(sed -n 's/.*rev = "\([0-9a-f]*\)".*/\1/p' rust/iets-ffi/Cargo.toml | head -1)

have_lib() {
  [ -f "$libdir/libiets_ffi.a" ] && [ "$(cat "$libdir/IETS_REV" 2>/dev/null)" = "$want" ]
}

tags=""
if [ "${OAR_NO_IETS:-}" != 1 ] && [ "$goos" != windows ]; then
  if ! have_lib; then
    hack/fetch-iets.sh "$goos/$goarch" || {
      if command -v cargo >/dev/null; then
        hack/build-iets.sh "$goos/$goarch"
      else
        echo "note: no prebuilt libiets_ffi.a for $goos/$goarch and no cargo to build it" >&2
      fi
    }
  fi
  if have_lib; then
    tags="iets,netgo,osusergo"
  else
    echo "note: building without the embedded nix evaluator (docs/development.md)" >&2
  fi
fi

if [ "$goos" = darwin ]; then
  CGO_ENABLED=1 go build -trimpath ${tags:+-tags "$tags"} -o bin/oar ./cmd/oar
  codesign --force --sign - --entitlements hack/vz.entitlements bin/oar
elif [ -n "$tags" ]; then
  # the evaluator is a static library; keep the binary fully static as before
  CGO_ENABLED=1 go build -trimpath -tags "$tags" \
    -ldflags '-linkmode external -extldflags "-static"' -o bin/oar ./cmd/oar
else
  CGO_ENABLED=0 go build -trimpath -o bin/oar ./cmd/oar
fi
echo "built $ROOT/bin/oar ($(bin/oar --version | sed -n 's/^nix evaluator: //p'))"
