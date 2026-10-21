//go:build linux

package agent

import (
	"fmt"
	"os"
	"strconv"
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
	if switchedRoot() {
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
	// the re-executed init learns through its arguments that it runs on the
	// switched root, and when the kernel handed over and the switch ended
	args := []string{"/init", switchedMark, fmt.Sprintf("%f", start), fmt.Sprintf("%f", uptime())}
	err := syscall.Exec("/init", args, os.Environ())
	logInit("switch_root: exec: %v", err)
}

// switchedMark is the first argument of an init re-executed by switchRoot
// (the kernel passes unknown command-line words to init as arguments, so
// the mark must not look like one).
const switchedMark = "oar-switched-root"

func switchedRoot() bool { return len(os.Args) > 1 && os.Args[1] == switchedMark }

// switchTimes returns the uptimes switchRoot passed on: kernel handover and
// end of the switch.
func switchTimes() (kernel, switched float64, ok bool) {
	if !switchedRoot() || len(os.Args) < 4 {
		return 0, 0, false
	}
	k, err1 := strconv.ParseFloat(os.Args[2], 64)
	s, err2 := strconv.ParseFloat(os.Args[3], 64)
	return k, s, err1 == nil && err2 == nil
}
