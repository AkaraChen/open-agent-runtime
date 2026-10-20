//go:build linux

package agent

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const ramfsMagic = 0x858458f6

// switchRoot moves PID 1 from the kernel's initramfs (rootfs, which cannot
// be pivot_root'ed away from) onto a tmpfs copy of it and re-executes
// /init there. A pivotable root lets runc use pivot_root instead of
// --no-pivot, and lets CRIU restore containers (it pivots as well).
// It returns only if no switch is needed or it failed.
func switchRoot() {
	if os.Getenv("OAR_SWITCHED_ROOT") == "1" {
		return
	}
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil || (st.Type != ramfsMagic && st.Type != unix.TMPFS_MAGIC) {
		return
	}
	if err := os.MkdirAll("/newroot", 0o755); err != nil {
		return
	}
	if err := unix.Mount("tmpfs", "/newroot", "tmpfs", 0, "mode=0755"); err != nil {
		logInit("switch_root: mount tmpfs: %v", err)
		return
	}
	if err := copyTree("/", "/newroot"); err != nil {
		logInit("switch_root: copy: %v", err)
		unix.Unmount("/newroot", unix.MNT_DETACH)
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
	env := append(os.Environ(), "OAR_SWITCHED_ROOT=1")
	err := syscall.Exec("/init", []string{"/init"}, env)
	logInit("switch_root: exec: %v", err)
}

// copyTree copies the initramfs into dst, skipping dst itself and pseudo
// filesystem mountpoints.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dst || p == "/proc" || p == "/sys" || p == "/dev" {
			if d.IsDir() {
				os.MkdirAll(filepath.Join(dst, p), 0o755)
			}
			return fs.SkipDir
		}
		target := filepath.Join(dst, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(l, target); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
			if err != nil {
				in.Close()
				return err
			}
			_, err = io.Copy(out, in)
			in.Close()
			out.Close()
			if err != nil {
				return err
			}
			os.Chmod(target, info.Mode())
		default:
			return nil // device nodes are recreated by devtmpfs
		}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			os.Lchown(target, int(sys.Uid), int(sys.Gid))
		}
		return nil
	})
}
