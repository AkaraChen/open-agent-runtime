//go:build linux

package agent

import (
	"archive/tar"
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
func extractTar(r io.Reader, dest string) error {
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
		if hdr.Typeflag != tar.TypeDir {
			if st, err := os.Lstat(full); err == nil {
				if st.IsDir() {
					if err := os.RemoveAll(full); err != nil {
						return err
					}
				} else if err := os.Remove(full); err != nil {
					return err
				}
				delete(safeDirs, rel)
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := ensureDir(rel); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{full, mode, hdr})
			continue
		case tar.TypeReg, tar.TypeRegA:
			f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|unix.O_NOFOLLOW, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, full); err != nil {
				return err
			}
		case tar.TypeLink:
			target := path.Clean("/" + hdr.Linkname)[1:]
			tfull := filepath.Join(dest, target)
			if st, err := os.Lstat(tfull); err != nil || st.IsDir() {
				return fmt.Errorf("bad hardlink %s -> %s", rel, hdr.Linkname)
			}
			if err := os.Link(tfull, full); err != nil {
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
			if err := unix.Mknod(full, m|uint32(mode.Perm()), int(dev)); err != nil {
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
