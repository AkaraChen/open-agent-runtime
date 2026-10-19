#!/bin/sh
# Runs inside an alpine container: assembles the guest userland for $APKARCH
# without executing foreign binaries, then packs it as initramfs and tar.
set -eu
apk add --no-cache cpio tar >/dev/null
R=/tmp/rootfs
rm -rf "$R" && mkdir -p "$R/etc/apk"
apk --arch "$APKARCH" --root "$R" --initdb --no-scripts --no-cache \
    --keys-dir "/usr/share/apk/keys/$APKARCH" \
    -X "https://dl-cdn.alpinelinux.org/alpine/$ALPINE_BRANCH/main" \
    add alpine-baselayout busybox musl e2fsprogs ca-certificates-bundle >/dev/null
for a in sh ash ls cat mount umount mkdir ps dmesg echo sleep kill ln rm cp mv df du free top uname vi grep sed awk tar find chmod chown head tail wc; do
  ln -sf /bin/busybox "$R/bin/$a"
done
for a in ip ifconfig route udhcpc poweroff reboot halt; do
  ln -sf /bin/busybox "$R/sbin/$a"
done
install -m 0755 /in/oar-agent "$R/init"
ln -sf /init "$R/usr/bin/oar-agent"
install -m 0755 /in/runc "$R/usr/bin/runc"
mkdir -p "$R/proc" "$R/sys" "$R/dev" "$R/run" "$R/tmp" "$R/var/lib/oar"
echo oar-guest > "$R/etc/hostname"
test -x "$R/sbin/mkfs.ext4" || test -e "$R/sbin/mkfs.ext4"
cd "$R"
find . | sort | cpio -o -H newc -R 0:0 --quiet | gzip -9n > /out/initramfs.cpio.gz
tar --numeric-owner --owner=0 --group=0 -cf /out/rootfs.tar .
chown "$OWNER" /out/initramfs.cpio.gz /out/rootfs.tar
echo ">> initramfs: $(du -h /out/initramfs.cpio.gz | cut -f1)"
