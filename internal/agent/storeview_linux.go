//go:build linux

package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
)

// maxStoreViews bounds how many bundle views stay mounted. Views of bundles
// with live containers are never unmounted.
const maxStoreViews = 8

// storeViews tracks per-bundle /nix/store views mounted by this agent.
type storeViews struct {
	mu    sync.Mutex
	views map[string]*storeView // keyed by view directory
}

type storeView struct {
	digest string
	used   time.Time
}

// ensureStoreView mounts every closure path of b read-only onto the
// bundle's skeleton, once per bundle. Containers then get the whole view
// with a single rbind, so their start cost does not depend on closure size.
func (s *Server) ensureStoreView(b *compose.Bundle) error {
	if b.StoreView == "" {
		return nil
	}
	sv := &s.views
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if sv.views == nil {
		sv.views = map[string]*storeView{}
	}
	if v, ok := sv.views[b.StoreView]; ok {
		v.used = time.Now()
		return nil
	}
	// a view left mounted by a previous agent (possibly half built) is
	// rebuilt; running containers keep their own copy of the mounts
	if isMountRoot(b.StoreView) {
		unix.Unmount(b.StoreView, unix.MNT_DETACH)
	}
	if err := s.mountStoreView(b); err != nil {
		unix.Unmount(b.StoreView, unix.MNT_DETACH)
		return err
	}
	sv.views[b.StoreView] = &storeView{digest: b.Digest, used: time.Now()}
	s.pruneStoreViewsLocked()
	return nil
}

// mountStoreView makes the skeleton a mount of its own, binds the store
// paths below it and makes the whole tree read-only with one recursive
// mount_setattr, so building a view takes one syscall per path and
// removing it a single detach.
func (s *Server) mountStoreView(b *compose.Bundle) error {
	view := b.StoreView
	if err := unix.Mount(view, view, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("mount store view: %w", err)
	}
	for _, name := range b.StorePaths {
		if err := unix.Mount(s.layout.StorePath(name), filepath.Join(view, name), "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("mount store path %s: %w", name, err)
		}
	}
	attr := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV}
	err := unix.MountSetattr(-1, view, unix.AT_RECURSIVE, attr)
	if err != unix.ENOSYS {
		return err
	}
	// kernels before 5.12: remount each mount
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
	for _, dst := range append([]string{view}, b.StorePaths...) {
		if dst != view {
			dst = filepath.Join(view, dst)
		}
		if err := unix.Mount("", dst, "", flags, ""); err != nil {
			return fmt.Errorf("make %s read-only: %w", dst, err)
		}
	}
	return nil
}

// pruneStoreViewsLocked unmounts the least recently used views beyond the
// limit, skipping views that live containers depend on.
func (s *Server) pruneStoreViewsLocked() {
	sv := &s.views
	if len(sv.views) <= maxStoreViews {
		return
	}
	inUse := map[string]bool{}
	if list, err := s.loadInstances(); err == nil {
		for _, in := range list {
			if !in.Retired {
				inUse[in.BundleDigest] = true
			}
		}
	}
	type cand struct {
		dir string
		v   *storeView
	}
	var cands []cand
	for dir, v := range sv.views {
		if !inUse[v.digest] {
			cands = append(cands, cand{dir, v})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].v.used.Before(cands[j].v.used) })
	for _, c := range cands {
		if len(sv.views) <= maxStoreViews {
			return
		}
		unix.Unmount(c.dir, unix.MNT_DETACH)
		delete(sv.views, c.dir)
	}
}

// makePrivate gives dir private mount propagation, bind-mounting it onto
// itself first if it is not a mount point, and reports whether it did that.
// Otherwise every store view and instance overlay mounted below it is
// copied into each mount namespace that is a peer or slave of the parent
// mount (on a systemd host: every service with PrivateTmp or
// ProtectSystem), which multiplies the cost of each mount and unmount and
// leaks the mounts into those namespaces. Best effort: needs
// CAP_SYS_ADMIN.
func makePrivate(dir string) bool {
	bound := false
	if !isMountRoot(dir) {
		if unix.Mount(dir, dir, "", unix.MS_BIND, "") != nil {
			return false
		}
		bound = true
	}
	unix.Mount("", dir, "", unix.MS_REC|unix.MS_PRIVATE, "")
	return bound
}

// isMountRoot reports whether path is the root of a mount, with one statx
// call (reading mountinfo costs ~0.5 ms per read on a host with a few
// hundred mounts); before Linux 5.8 it falls back to mountinfo.
func isMountRoot(path string) bool {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, 0, &st); err != nil {
		return false
	}
	if st.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 {
		return st.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real // mountinfo shows resolved paths
	}
	return isMountpoint(path)
}

// isMountpoint reports whether dir is a mount point.
func isMountpoint(dir string) bool {
	found := false
	eachMountpoint(func(mp string) {
		if mp == dir {
			found = true
		}
	})
	return found
}

// mountpointsUnder returns the mount points below dir.
func mountpointsUnder(dir string) (map[string]bool, error) {
	prefix := filepath.Clean(dir) + "/"
	out := map[string]bool{}
	err := eachMountpoint(func(mp string) {
		if strings.HasPrefix(mp, prefix) {
			out[mp] = true
		}
	})
	return out, err
}

// eachMountpoint calls fn with every mount point of this namespace.
func eachMountpoint(fn func(string)) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		fn(unescapeMountinfo(fields[4]))
	}
	return sc.Err()
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

// unmountAllStoreViews removes every store view mount (agent shutdown).
// Running containers keep their own copies of the mounts.
func (s *Server) unmountAllStoreViews() {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	mps, err := mountpointsUnder(filepath.Join(s.cfg.Root, "bundles"))
	if err == nil {
		for mp := range mps {
			unix.Unmount(mp, unix.MNT_DETACH)
		}
	}
	s.views.views = nil
}
