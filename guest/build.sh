#!/usr/bin/env bash
# Builds the minimal Linux guest: kernel + initramfs (init/agent, runc, e2fsprogs).
#
#   guest/build.sh <amd64|arm64> [kernel|initramfs|all]
#
# Outputs guest/out/<arch>/{kernel,initramfs.cpio.lz4,rootfs.tar}. Requires
# docker and go. Builds are cached in guest/.cache.
set -euo pipefail

ARCH=${1:?usage: build.sh <amd64|arm64> [kernel|initramfs|all]}
WHAT=${2:-all}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/guest/out/$ARCH
CACHE=$ROOT/guest/.cache
KERNEL_VERSION=${KERNEL_VERSION:-6.12.113}
RUNC_VERSION=${RUNC_VERSION:-v1.5.2}
ALPINE_BRANCH=${ALPINE_BRANCH:-v3.22}
mkdir -p "$OUT" "$CACHE"

case $ARCH in
  amd64) KARCH=x86_64; APKARCH=x86_64; KIMAGE=bzImage; KPATH=arch/x86/boot/bzImage ;;
  arm64) KARCH=arm64;  APKARCH=aarch64; KIMAGE=Image;  KPATH=arch/arm64/boot/Image ;;
  *) echo "unsupported arch $ARCH" >&2; exit 1 ;;
esac

kernel() {
  docker build -q -t oar-guest-builder "$ROOT/guest" >/dev/null
  docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -v "$CACHE:/cache" -v "$ROOT/guest:/guest:ro" -v "$OUT:/out" \
    -e KERNEL_VERSION="$KERNEL_VERSION" -e KARCH="$KARCH" -e ARCH_NAME="$ARCH" \
    -e KIMAGE="$KIMAGE" -e KPATH="$KPATH" \
    oar-guest-builder bash /guest/build-kernel.sh
}

initramfs() {
  echo ">> building oar-agent for linux/$ARCH"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -ldflags="-s -w" \
      -o "$CACHE/in-$ARCH/oar-agent" ./cmd/oar-agent)
  local runc="$CACHE/runc-$RUNC_VERSION.$ARCH"
  if [ ! -s "$runc" ]; then
    echo ">> downloading runc $RUNC_VERSION ($ARCH)"
    curl -fsSL -o "$runc.tmp" "https://github.com/opencontainers/runc/releases/download/$RUNC_VERSION/runc.$ARCH"
    mv "$runc.tmp" "$runc"
  fi
  cp "$runc" "$CACHE/in-$ARCH/runc"
  docker run --rm -v "$CACHE/in-$ARCH:/in:ro" -v "$ROOT/guest:/guest:ro" -v "$OUT:/out" \
    -e APKARCH="$APKARCH" -e ALPINE_BRANCH="$ALPINE_BRANCH" -e OWNER="$(id -u):$(id -g)" \
    "alpine:${ALPINE_BRANCH#v}" sh /guest/build-rootfs.sh
}

case $WHAT in
  kernel) kernel ;;
  initramfs) initramfs ;;
  all) kernel; initramfs ;;
  *) echo "unknown target $WHAT" >&2; exit 1 ;;
esac
ls -la "$OUT"
