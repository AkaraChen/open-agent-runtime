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
	n := 0
	err := extractTarCount(r, dest, &n)
	return n, err
}

// maxDirFds bounds the directory descriptors kept open while extracting.
const maxDirFds = 256

// extractor holds the state of one extraction. Each path component is
// resolved once: directories are opened with openat (O_NOFOLLOW, so a
// symlink is never traversed) and cached, and entries are created relative
// to their parent's descriptor instead of by full path.
type extractor struct {
	fds   map[string]int      // open directories by relative path ("" is dest)
	known map[string]int      // verified directories: their permission bits now (-1: unknown)
	files map[string]bool     // non-directory entries created (hard link targets)
	dirs  map[string]*dirMeta // explicit directory entries, finished last
}

type dirMeta struct {
	mode os.FileMode
	hdr  *tar.Header
}

func (e *extractor) closeAll() {
	for _, fd := range e.fds {
		unix.Close(fd)
	}
}

// trim drops the cached descriptors (but the root) when there are many.
func (e *extractor) trim() {
	if len(e.fds) <= maxDirFds {
		return
	}
	for rel, fd := range e.fds {
		if rel != "" {
			unix.Close(fd)
			delete(e.fds, rel)
		}
	}
}

// dirFd returns a descriptor for the directory rel below dest. Missing
// directories are created (0755) when create is set; anything that is not
// a real directory (a symlink in particular) is refused.
func (e *extractor) dirFd(rel string, create bool) (int, error) {
	if fd, ok := e.fds[rel]; ok {
		return fd, nil
	}
	pfd, err := e.dirFd(parentOf(rel), create)
	if err != nil {
		return -1, err
	}
	name := path.Base(rel)
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(pfd, name, flags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		if err = unix.Mkdirat(pfd, name, 0o755); err == nil {
			e.known[rel] = 0o755 &^ int(umask)
			fd, err = unix.Openat(pfd, name, flags, 0)
		}
	} else if err == nil {
		if _, ok := e.known[rel]; !ok {
			e.known[rel] = -1
		}
	}
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return -1, fmt.Errorf("refusing to traverse non-directory %s", rel)
		}
		return -1, err
	}
	e.fds[rel] = fd
	return fd, nil
}

// forget drops everything remembered about rel and below it, once the
// entry was replaced.
func (e *extractor) forget(rel string) {
	prefix := rel + "/"
	below := func(k string) bool { return k == rel || strings.HasPrefix(k, prefix) }
	for k, fd := range e.fds {
		if below(k) {
			unix.Close(fd)
			delete(e.fds, k)
		}
	}
	for k := range e.known {
		if below(k) {
			delete(e.known, k)
		}
	}
	for k := range e.files {
		if below(k) {
			delete(e.files, k)
		}
	}
	for k := range e.dirs {
		if below(k) {
			delete(e.dirs, k)
		}
	}
}

func extractTarCount(r io.Reader, dest string, count *int) error {
	tr := tar.NewReader(r)
	root, err := unix.Open(dest, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	e := &extractor{
		fds:   map[string]int{"": root},
		known: map[string]int{"": -1},
		files: map[string]bool{},
		dirs:  map[string]*dirMeta{},
	}
	defer e.closeAll()

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
		e.trim()
		pfd, err := e.dirFd(parentOf(rel), true)
		if err != nil {
			return err
		}
		name := path.Base(rel)
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
			var st unix.Stat_t
			if err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			if st.Mode&unix.S_IFMT == unix.S_IFDIR {
				err = os.RemoveAll(full)
			} else {
				err = unix.Unlinkat(pfd, name, 0)
			}
			e.forget(rel)
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
			// as root a directory gets its final mode at once (it can be
			// written to regardless); otherwise it stays owner-writable
			// until the end
			perm := mode.Perm()
			if !isRoot {
				perm |= 0o700
			}
			switch err := unix.Mkdirat(pfd, name, uint32(perm)); {
			case err == nil:
				e.known[rel] = int(perm) &^ int(umask)
			case errors.Is(err, os.ErrExist):
				if _, err := e.dirFd(rel, false); err != nil {
					return err
				}
			default:
				return err
			}
			e.dirs[rel] = &dirMeta{mode, hdr}
			continue
		case tar.TypeReg, tar.TypeRegA:
			var f *os.File
			if err := retry(func() error {
				fd, err := unix.Openat(pfd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
				if err == nil {
					f = os.NewFile(uintptr(fd), full)
				}
				return err
			}); err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			err := fileMeta(f, hdr, mode)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			e.files[rel] = true
			continue // metadata applied on the open file
		case tar.TypeSymlink:
			if err := retry(func() error { return unix.Symlinkat(hdr.Linkname, pfd, name) }); err != nil {
				return err
			}
		case tar.TypeLink:
			target := path.Clean("/" + hdr.Linkname)[1:]
			tname := path.Base(target)
			tpfd, err := e.dirFd(parentOf(target), false)
			if err != nil {
				return fmt.Errorf("bad hardlink %s -> %s", rel, hdr.Linkname)
			}
			if !e.files[target] { // not made by this extraction: check it
				var st unix.Stat_t
				if err := unix.Fstatat(tpfd, tname, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil || st.Mode&unix.S_IFMT == unix.S_IFDIR {
					return fmt.Errorf("bad hardlink %s -> %s", rel, hdr.Linkname)
				}
			}
			if err := retry(func() error { return unix.Linkat(tpfd, tname, pfd, name, 0) }); err != nil {
				return err
			}
			e.files[rel] = true
			continue // shares metadata with the target
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			m := uint32(unix.S_IFIFO)
			if hdr.Typeflag == tar.TypeChar {
				m = unix.S_IFCHR
			} else if hdr.Typeflag == tar.TypeBlock {
				m = unix.S_IFBLK
			}
			dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
			if err := retry(func() error { return unix.Mknodat(pfd, name, m|uint32(mode.Perm()), int(dev)) }); err != nil {
				return err
			}
		default:
			continue
		}
		e.files[rel] = true
		if err := nodeMeta(pfd, name, full, hdr, mode, int(mode.Perm())&^int(umask), hdr.Typeflag == tar.TypeSymlink); err != nil {
			return err
		}
	}
	// directories last, children before parents (reverse path order), so
	// modes without owner access don't block what is below
	rels := make([]string, 0, len(e.dirs))
	for rel := range e.dirs {
		rels = append(rels, rel)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(rels)))
	for _, rel := range rels {
		d := e.dirs[rel]
		e.trim()
		pfd, err := e.dirFd(parentOf(rel), false)
		if err != nil {
			return err
		}
		if err := nodeMeta(pfd, path.Base(rel), filepath.Join(dest, rel), d.hdr, d.mode, e.known[rel], false); err != nil {
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

// The effective ids, read once: files and directories are created with
// them, so ownership only needs setting when the tar says otherwise. Root
// (CAP_DAC_OVERRIDE) can create entries below directories of any mode.
var (
	euid, egid = os.Geteuid(), os.Getegid()
	isRoot     = euid == 0
)

// fileMeta applies a regular file's metadata through its open descriptor:
// no path lookups, and no chown or chmod when creation already got them
// right (the common case: root-owned files whose mode the umask keeps).
func fileMeta(f *os.File, hdr *tar.Header, mode os.FileMode) error {
	fd := int(f.Fd())
	chowned := false
	if hdr.Uid != euid || hdr.Gid != egid {
		if err := unix.Fchown(fd, hdr.Uid, hdr.Gid); err != nil && !errors.Is(err, unix.EPERM) {
			return err
		}
		chowned = true // also clears setuid/setgid bits
	}
	for k, v := range hdr.PAXRecords {
		if name, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			_ = unix.Fsetxattr(fd, name, []byte(v), 0) // best effort, as nodeMeta
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

// nodeMeta applies the metadata of the entry name below the directory pfd
// (a symlink, device, fifo or directory), by name relative to pfd and
// without following a final symlink. cur is the permission bits the entry
// has now, or -1 if unknown (it then also gets chowned and chmoded);
// ownership and mode are only set when they differ.
func nodeMeta(pfd int, name, full string, hdr *tar.Header, mode os.FileMode, cur int, symlink bool) error {
	if hdr.Uid != euid || hdr.Gid != egid || cur < 0 {
		if err := unix.Fchownat(pfd, name, hdr.Uid, hdr.Gid, unix.AT_SYMLINK_NOFOLLOW); err != nil && !errors.Is(err, unix.EPERM) {
			return err
		}
	}
	for k, v := range hdr.PAXRecords {
		if xname, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			// best effort: some filesystems reject security.* or trusted.*
			_ = unix.Lsetxattr(full, xname, []byte(v), 0)
		}
	}
	if !symlink && (cur != int(mode.Perm()) || mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0) {
		m := uint32(mode.Perm())
		if mode&os.ModeSetuid != 0 {
			m |= unix.S_ISUID
		}
		if mode&os.ModeSetgid != 0 {
			m |= unix.S_ISGID
		}
		if mode&os.ModeSticky != 0 {
			m |= unix.S_ISVTX
		}
		if err := unix.Fchmodat(pfd, name, m, 0); err != nil {
			return err
		}
	}
	ts := []unix.Timespec{unix.NsecToTimespec(hdr.ModTime.UnixNano()), unix.NsecToTimespec(hdr.ModTime.UnixNano())}
	if !hdr.AccessTime.IsZero() {
		ts[0] = unix.NsecToTimespec(hdr.AccessTime.UnixNano())
	}
	return unix.UtimesNanoAt(pfd, name, ts, unix.AT_SYMLINK_NOFOLLOW)
}
