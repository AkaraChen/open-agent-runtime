//go:build linux

package agent

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const ramfsMagic = 0x858458f6

// switchRoot moves PID 1 off the kernel's initramfs (rootfs, which cannot
// be pivot_root'ed away from) onto a bind mount of it, and re-executes
// /init there. A pivotable root lets runc use pivot_root instead of
// --no-pivot, and lets CRIU restore containers (it pivots as well). The
// bind mount has a parent mount, which is all pivot_root needs, so nothing
// is copied. It returns only if no switch is needed or it failed.
func switchRoot() {
	if os.Getenv("OAR_SWITCHED_ROOT") == "1" {
		return
	}
	start := uptime()
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil || (st.Type != ramfsMagic && st.Type != unix.TMPFS_MAGIC) {
		return
	}
	if err := os.MkdirAll("/newroot", 0o755); err != nil {
		return
	}
	if err := unix.Mount("/", "/newroot", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		logInit("switch_root: bind: %v", err)
		return
	}
	if err := os.Chdir("/newroot"); err != nil {
		return
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		logInit("switch_root: move: %v", err)
		return
	}
	if err := unix.Chroot("."); err != nil {
		logInit("switch_root: chroot: %v", err)
		return
	}
	os.Chdir("/")
	env := append(os.Environ(), "OAR_SWITCHED_ROOT=1",
		fmt.Sprintf("OAR_BOOT_KERNEL=%f", start), fmt.Sprintf("OAR_BOOT_SWITCHED=%f", uptime()))
	err := syscall.Exec("/init", []string{"/init"}, env)
	logInit("switch_root: exec: %v", err)
}
