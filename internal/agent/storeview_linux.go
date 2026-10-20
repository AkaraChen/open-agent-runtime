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
	paths  []string
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
	// after an agent restart the mounts may already exist
	mounted, err := mountpointsUnder(b.StoreView)
	if err != nil {
		return err
	}
	for _, name := range b.StorePaths {
		dst := filepath.Join(b.StoreView, name)
		if mounted[dst] {
			continue
		}
		src := s.layout.StorePath(name)
		if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("mount store path %s: %w", name, err)
		}
		flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
		if err := unix.Mount("", dst, "", flags, ""); err != nil {
			return fmt.Errorf("make store path %s read-only: %w", name, err)
		}
	}
	sv.views[b.StoreView] = &storeView{digest: b.Digest, paths: b.StorePaths, used: time.Now()}
	s.pruneStoreViewsLocked()
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
		for _, name := range c.v.paths {
			unix.Unmount(filepath.Join(c.dir, name), unix.MNT_DETACH)
		}
		delete(sv.views, c.dir)
	}
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

// unmountAllStoreViews removes every store view mount (guest shutdown).
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
