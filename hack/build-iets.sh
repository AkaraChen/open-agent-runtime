#!/usr/bin/env bash
# Builds the iets static library for one Go platform and installs it where
# the cgo binding looks (internal/nixeval/iets/lib/<goos>_<goarch>/).
#
#   hack/build-iets.sh                 # the host platform
#   hack/build-iets.sh linux/arm64     # cross-compile (needs the Rust target
#                                      # and a linker for it)
#
# Needs cargo; rust/iets-ffi/rust-toolchain.toml pins the toolchain. The
# iets revision is pinned in rust/iets-ffi/Cargo.toml.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
CRATE="$ROOT/rust/iets-ffi"

platform=${1:-}
if [ -z "$platform" ]; then
  platform="$(go env GOOS)/$(go env GOARCH)"
fi
goos=${platform%/*}
goarch=${platform#*/}

# OAR_IETS_TARGET overrides the Rust target, e.g. x86_64-unknown-linux-musl
# for a fully static Linux binary (needs musl-gcc or zig as the C linker).
target=${OAR_IETS_TARGET:-}
if [ -z "$target" ]; then
  case "$goos/$goarch" in
    linux/amd64)  target=x86_64-unknown-linux-gnu ;;
    linux/arm64)  target=aarch64-unknown-linux-gnu ;;
    darwin/arm64) target=aarch64-apple-darwin ;;
    darwin/amd64) target=x86_64-apple-darwin ;;
    *) echo "unsupported platform $platform" >&2; exit 1 ;;
  esac
fi

echo "building libiets_ffi for $platform ($target)"
(cd "$CRATE" && cargo build --release --target "$target")

dest="$ROOT/internal/nixeval/iets/lib/${goos}_${goarch}"
mkdir -p "$dest"
cp "$CRATE/target/$target/release/libiets_ffi.a" "$dest/libiets_ffi.a"
rev=$(sed -n 's/.*rev = "\([0-9a-f]*\)".*/\1/p' "$CRATE/Cargo.toml" | head -1)
printf '%s\n' "$rev" > "$dest/IETS_REV"
if command -v sha256sum >/dev/null; then
  sha256sum "$dest/libiets_ffi.a" | cut -d' ' -f1 > "$dest/SHA256"
else
  shasum -a 256 "$dest/libiets_ffi.a" | cut -d' ' -f1 > "$dest/SHA256"
fi
echo "installed $dest/libiets_ffi.a ($(du -h "$dest/libiets_ffi.a" | cut -f1), iets $rev)"
