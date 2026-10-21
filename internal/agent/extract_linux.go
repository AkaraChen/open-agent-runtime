//go:build linux

package agent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// extractTar unpacks a tar stream into dest (which must exist). It preserves
// ownership, permissions, xattrs, device nodes and timestamps, and refuses to
// write through symlinks or outside dest.
func extractTar(r io.Reader, dest string) (int, error) {
	return extractTarOpt(r, dest, false)
}

// writebackMin is the smallest file whose writeback extractTarOpt starts
// at once: smaller ones would turn into many tiny writes.
const writebackMin = 64 << 10

// extractTarOpt is extractTar; with writeback it starts writing each
// larger file's data back as soon as it is extracted (asynchronously:
// sync_file_range without waiting), so disk writes overlap the rest of
// the warm instead of piling up for a final flush.
func extractTarOpt(r io.Reader, dest string, writeback bool) (int, error) {
	n := 0
	err := extractTarCount(r, dest, &n, writeback)
	return n, err
}

func extractTarCount(r io.Reader, dest string, count *int, writeback bool) error {
	tr := tar.NewReader(r)
	type dirMeta struct {
		path string
		mode os.FileMode
		hdr  *tar.Header
	}
	var dirs []dirMeta
	safeDirs := map[string]bool{"": true}

	// ensureParent creates missing parents and verifies existing ones are
	// real directories (not symlinks).
	var ensureDir func(rel string) error
	ensureDir = func(rel string) error {
		if safeDirs[rel] {
			return nil
		}
		if err := ensureDir(parentOf(rel)); err != nil {
			return err
		}
		full := filepath.Join(dest, rel)
		st, err := os.Lstat(full)
		switch {
		case err == nil && st.IsDir():
		case err == nil:
			return fmt.Errorf("refusing to traverse non-directory %s", rel)
		case os.IsNotExist(err):
			if err := os.Mkdir(full, 0o755); err != nil {
				return err
			}
		default:
			return err
		}
		safeDirs[rel] = true
		return nil
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel := path.Clean("/" + hdr.Name)[1:]
		if rel == "" {
			continue
		}
		*count++
		if err := ensureDir(parentOf(rel)); err != nil {
			return err
		}
		full := filepath.Join(dest, rel)
		mode := os.FileMode(hdr.Mode).Perm()
		if hdr.Mode&04000 != 0 {
			mode |= os.ModeSetuid
		}
		if hdr.Mode&02000 != 0 {
			mode |= os.ModeSetgid
		}
		if hdr.Mode&01000 != 0 {
			mode |= os.ModeSticky
		}
		// entries normally land on fresh paths: creation fails with EEXIST
		// only for duplicates, which then replace what is there (no lstat
		// per entry)
		replace := func() error {
			st, err := os.Lstat(full)
			if err != nil {
				return err
			}
			if st.IsDir() {
				err = os.RemoveAll(full)
			} else {
				err = os.Remove(full)
			}
			delete(safeDirs, rel)
			return err
		}
		retry := func(create func() error) error {
			err := create()
			if errors.Is(err, os.ErrExist) {
				if err := replace(); err != nil {
					return err
				}
				err = create()
			}
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := ensureDir(rel); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{full, mode, hdr})
			continue
		case tar.TypeReg, tar.TypeRegA:
			var f *os.File
			if err := retry(func() (err error) {
				f, err = os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, mode.Perm())
				return err
			}); err != nil {
				return err
			}
			written, err := io.Copy(f, tr)
			if err != nil {
				f.Close()
				return err
			}
			if writeback && written >= writebackMin {
				unix.SyncFileRange(int(f.Fd()), 0, 0, unix.SYNC_FILE_RANGE_WRITE)
			}
			err = fileMeta(f, hdr, mode)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			continue // metadata applied on the open file
		case tar.TypeSymlink:
			if err := retry(func() error { return os.Symlink(hdr.Linkname, full) }); err != nil {
				return err
			}
		case tar.TypeLink:
			target := path.Clean("/" + hdr.Linkname)[1:]
			tfull := filepath.Join(dest, target)
			if st, err := os.Lstat(tfull); err != nil || st.IsDir() {
				return fmt.Errorf("bad hardlink %s -> %s", rel, hdr.Linkname)
			}
			if err := retry(func() error { return os.Link(tfull, full) }); err != nil {
				return err
			}
			continue // shares metadata with the target
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			m := uint32(unix.S_IFIFO)
			if hdr.Typeflag == tar.TypeChar {
				m = unix.S_IFCHR
			} else if hdr.Typeflag == tar.TypeBlock {
				m = unix.S_IFBLK
			}
			dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
			if err := retry(func() error { return unix.Mknod(full, m|uint32(mode.Perm()), int(dev)) }); err != nil {
				return err
			}
		default:
			continue
		}
		if err := applyMeta(full, hdr, mode, hdr.Typeflag == tar.TypeSymlink); err != nil {
			return err
		}
	}
	// directories last, deepest first, so read-only dirs don't block children
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := applyMeta(d.path, d.hdr, d.mode, false); err != nil {
			return err
		}
	}
	return nil
}

func parentOf(rel string) string {
	d := path.Dir(rel)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// umask is the process umask, read once (it is process-wide).
var umask = func() os.FileMode {
	m := unix.Umask(0)
	unix.Umask(m)
	return os.FileMode(m)
}()

// fileMeta applies a regular file's metadata through its open descriptor:
// no path lookups, and no chown or chmod when creation already got them
// right (the common case: root-owned files whose mode the umask keeps).
func fileMeta(f *os.File, hdr *tar.Header, mode os.FileMode) error {
	fd := int(f.Fd())
	chowned := false
	if hdr.Uid != os.Geteuid() || hdr.Gid != os.Getegid() {
		if err := unix.Fchown(fd, hdr.Uid, hdr.Gid); err != nil && !errors.Is(err, unix.EPERM) {
			return err
		}
		chowned = true // also clears setuid/setgid bits
	}
	for k, v := range hdr.PAXRecords {
		if name, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			_ = unix.Fsetxattr(fd, name, []byte(v), 0) // best effort, as applyMeta
		}
	}
	if chowned || mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || mode.Perm()&umask != 0 {
		if err := f.Chmod(mode); err != nil {
			return err
		}
	}
	atime := hdr.ModTime
	if !hdr.AccessTime.IsZero() {
		atime = hdr.AccessTime
	}
	return unix.Futimes(fd, []unix.Timeval{unix.NsecToTimeval(atime.UnixNano()), unix.NsecToTimeval(hdr.ModTime.UnixNano())})
}

func applyMeta(full string, hdr *tar.Header, mode os.FileMode, symlink bool) error {
	if err := os.Lchown(full, hdr.Uid, hdr.Gid); err != nil && !os.IsPermission(err) {
		return err
	}
	for k, v := range hdr.PAXRecords {
		if name, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			// best effort: some filesystems reject security.* or trusted.*
			_ = unix.Lsetxattr(full, name, []byte(v), 0)
		}
	}
	if !symlink {
		if err := os.Chmod(full, mode); err != nil {
			return err
		}
	}
	ts := []unix.Timespec{unix.NsecToTimespec(hdr.ModTime.UnixNano()), unix.NsecToTimespec(hdr.ModTime.UnixNano())}
	if !hdr.AccessTime.IsZero() {
		ts[0] = unix.NsecToTimespec(hdr.AccessTime.UnixNano())
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, full, ts, unix.AT_SYMLINK_NOFOLLOW)
}
