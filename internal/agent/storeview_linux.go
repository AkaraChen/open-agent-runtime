//go:build linux

package agent

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
)

// Each bundle's /nix/store view is a plain directory tree that mirrors its
// closure, with every file hard-linked to the shared store's inode. Reads
// go straight to the underlying filesystem (no overlay indirection), the
// page cache is shared between bundles, and no mounts stay resident in the
// agent's namespace (each container gets the view with one read-only
// bind). Building a view costs one mkdir per directory and one link per
// file, once per closure, off the start path (at warm time).

type storeGroups struct {
	mu      sync.Mutex
	mounted map[string]bool // views known to be complete
}

// ensureStoreView builds the bundle's /nix/store view if it is missing.
func (s *Server) ensureStoreView(b *compose.Bundle) error {
	if len(b.StoreLayers) == 0 {
		return nil
	}
	view := b.StoreView
	s.groups.mu.Lock()
	defer s.groups.mu.Unlock()
	if s.groups.mounted == nil {
		s.groups.mounted = map[string]bool{}
	}
	if s.groups.mounted[view] {
		return nil
	}
	// the marker is written after the view is complete; a view without it
	// is partial or left by an older layout (a mount point)
	done := view + ".done"
	if _, err := os.Stat(done); err == nil {
		s.groups.mounted[view] = true
		return nil
	}
	unix.Unmount(view, unix.MNT_DETACH)
	if err := os.RemoveAll(view); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(view), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(view), "store.tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// store paths are linked in parallel: the cost is one syscall per file
	work := make(chan string)
	errs := make(chan error, len(b.StoreLayers))
	var wg sync.WaitGroup
	for range min(runtime.NumCPU()*2, len(b.StoreLayers)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for layer := range work {
				name := filepath.Base(layer)
				var dirs []dirMode
				err := linkTree(filepath.Join(layer, "nix", "store", name), filepath.Join(tmp, name), &dirs)
				// read-only store directories get their modes after their contents
				for i := len(dirs) - 1; i >= 0; i-- {
					os.Chmod(dirs[i].path, dirs[i].mode)
				}
				if err != nil {
					errs <- fmt.Errorf("store view %s: %w", name, err)
				}
			}
		}()
	}
	for _, layer := range b.StoreLayers {
		work <- layer
	}
	close(work)
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	os.Chmod(tmp, 0o755)
	if err := os.Rename(tmp, view); err != nil {
		return err
	}
	if err := os.WriteFile(done, nil, 0o644); err != nil {
		return err
	}
	s.groups.mounted[view] = true
	return nil
}

type dirMode struct {
	path string
	mode os.FileMode
}

// linkTree mirrors src at dst: directories are created, files hard-linked
// and symlinks copied.
func linkTree(src, dst string, dirs *[]dirMode) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			if err := os.Mkdir(target, 0o755); err != nil {
				return err
			}
			*dirs = append(*dirs, dirMode{target, info.Mode().Perm()})
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		default:
			return os.Link(p, target)
		}
		return nil
	})
}

// unmountStoreGroups removes mounts left below the bundle cache by older
// versions (store views used to be mounts).
func (s *Server) unmountStoreGroups() {
	if mps, err := mountpointsUnder(filepath.Join(s.cfg.Root, "bundles")); err == nil {
		for mp := range mps {
			unix.Unmount(mp, unix.MNT_DETACH)
		}
	}
}

// mountOverlay mounts an overlay with the given lower layers (highest
// priority first). Without upper it is read-only. Long layer lists exceed
// the 4 KiB mount data limit, so layers are added one by one through the
// new mount API (fsconfig "lowerdir+", Linux 6.8+).
func mountOverlay(target string, lowers []string, upper, work string) error {
	data := "lowerdir=" + strings.Join(lowers, ":")
	if upper != "" {
		data += ",upperdir=" + upper + ",workdir=" + work
	}
	if len(data) < 3500 {
		flags := uintptr(0)
		if upper == "" {
			flags = unix.MS_RDONLY
		}
		return unix.Mount("overlay", target, "overlay", flags, data)
	}
	fd, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("fsopen overlay: %w", err)
	}
	defer unix.Close(fd)
	for _, l := range lowers {
		if err := unix.FsconfigSetString(fd, "lowerdir+", l); err != nil {
			return fmt.Errorf("overlay lowerdir+ %s (needs Linux 6.8+): %w", l, err)
		}
	}
	if upper != "" {
		if err := unix.FsconfigSetString(fd, "upperdir", upper); err != nil {
			return err
		}
		if err := unix.FsconfigSetString(fd, "workdir", work); err != nil {
			return err
		}
	}
	if err := unix.FsconfigCreate(fd); err != nil {
		return fmt.Errorf("create overlay with %d layers: %w", len(lowers), err)
	}
	attr := 0
	if upper == "" {
		attr = unix.MOUNT_ATTR_RDONLY
	}
	mfd, err := unix.Fsmount(fd, unix.FSMOUNT_CLOEXEC, attr)
	if err != nil {
		return fmt.Errorf("fsmount overlay: %w", err)
	}
	defer unix.Close(mfd)
	return unix.MoveMount(mfd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH)
}

// mountpointsUnder returns the mount points below dir.
func mountpointsUnder(dir string) (map[string]bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	prefix := filepath.Clean(dir) + "/"
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountinfo(fields[4])
		if strings.HasPrefix(mp, prefix) {
			out[mp] = true
		}
	}
	return out, sc.Err()
}

// unescapeMountinfo decodes the octal escapes (\040 etc.) used in mountinfo.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
