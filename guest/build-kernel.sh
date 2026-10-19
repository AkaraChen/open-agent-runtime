#!/usr/bin/env bash
# Runs inside the oar-guest-builder container.
set -euo pipefail
cd /cache
SRC=/cache/linux-$KERNEL_VERSION
if [ ! -d "$SRC" ]; then
  echo ">> downloading linux $KERNEL_VERSION"
  wget -q -O linux.tar.xz.tmp "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_VERSION%%.*}.x/linux-$KERNEL_VERSION.tar.xz"
  tar -xf linux.tar.xz.tmp && rm linux.tar.xz.tmp
fi
B=/cache/build-$ARCH_NAME-$KERNEL_VERSION
mkdir -p "$B"
CROSS=""
case "$(uname -m)/$KARCH" in
  x86_64/arm64) CROSS=aarch64-linux-gnu- ;;
  aarch64/x86_64) CROSS=x86_64-linux-gnu- ;;
esac
M="make -C $SRC O=$B ARCH=$KARCH CROSS_COMPILE=$CROSS"
$M defconfig >/dev/null
"$SRC/scripts/kconfig/merge_config.sh" -m -O "$B" "$B/.config" /guest/kernel.config "/guest/kernel-$ARCH_NAME.config" >/dev/null
$M olddefconfig >/dev/null
# verify the fragment took effect
for opt in CONFIG_VIRTIO_BLK CONFIG_VIRTIO_VSOCKETS CONFIG_OVERLAY_FS CONFIG_EXT4_FS CONFIG_VIRTIO_CONSOLE CONFIG_CGROUP_BPF CONFIG_VIRTIO_NET; do
  grep -q "^$opt=y" "$B/.config" || { echo "missing $opt=y" >&2; exit 1; }
done
echo ">> building $KIMAGE ($KARCH, $(nproc) jobs)"
$M -j"$(nproc)" "$KIMAGE" >/dev/null
cp "$B/$KPATH" /out/kernel
cp "$B/.config" /out/kernel.config
echo ">> kernel: $(du -h /out/kernel | cut -f1)"
