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
	"sync/atomic"
	"time"

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
	mounted map[string]bool       // views known to be complete
	builds  map[string]*viewBuild // views being built ahead of a precompose
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
	if vb := s.groups.builds[view]; vb != nil {
		// started by view.prepare during the warm: let it finish (with
		// groups.mu held, like a build done here; link jobs do not take it)
		delete(s.groups.builds, view)
		vb.linkAll(b.StoreLayers) // any layer it has not seen arrive
		vb.mu.Lock()
		if len(vb.pending) > 0 { // compose checked that all are warm
			if vb.err == nil {
				vb.err = fmt.Errorf("store view: %d store paths missing", len(vb.pending))
			}
			vb.pending = nil
			vb.finish()
		}
		vb.mu.Unlock()
		<-vb.done
		if vb.err == nil {
			return s.publishView(vb.tmp, view, view+".done")
		}
		os.RemoveAll(vb.tmp) // build it here instead
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
	moved := s.stealViews(tmp, b.StoreLayers, view)
	var rest []string
	for _, layer := range b.StoreLayers {
		if !moved[filepath.Base(layer)] {
			rest = append(rest, layer)
		}
	}
	// store paths are linked in parallel: the cost is one syscall per file.
	// PERF: they are handed out in the order of b.StoreLayers, biggest
	// first (compose.StoreLayers), so that the build ends with small ones
	// instead of waiting for a big one that was started late.
	work := make(chan string)
	errs := make(chan error, len(rest))
	var wg sync.WaitGroup
	linkJobs.Add(int32(len(rest)))
	for range min(runtime.NumCPU()*2, max(len(rest), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for layer := range work {
				err := linkStorePath(layer, tmp)
				linkJobs.Add(-1)
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	for _, layer := range rest {
		work <- layer
	}
	close(work)
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	return s.publishView(tmp, view, done)
}

// publishView moves a completed view into place (caller holds groups.mu).
func (s *Server) publishView(tmp, view, done string) error {
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

// linkStorePath links the store path in layer into the view being built
// under tmp. It is a variable so that tests can hold up the link jobs.
var linkStorePath = func(layer, tmp string) error {
	name := filepath.Base(layer)
	if err := linkTree(filepath.Join(layer, "nix", "store", name), filepath.Join(tmp, name)); err != nil {
		return fmt.Errorf("store view %s: %w", name, err)
	}
	return nil
}

// linkTree mirrors src at dst: directories are created, files hard-linked
// and symlinks copied. It works relative to directory descriptors (the
// kernel resolves one path component per call). As root each directory is
// created with its final mode, since root can still add entries to a
// read-only one; otherwise directories are made owner-writable and get
// their modes once their contents are done.
func linkTree(src, dst string) error {
	sfd, err := unix.Open(src, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstat(sfd, &st); err != nil {
		unix.Close(sfd)
		return err
	}
	perm := os.FileMode(st.Mode).Perm()
	mk := perm
	if !isRoot {
		mk |= 0o700
	}
	if err := os.Mkdir(dst, mk); err != nil {
		unix.Close(sfd)
		return err
	}
	dfd, err := unix.Open(dst, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(sfd)
		return err
	}
	return linkDir(sfd, dfd, perm)
}

// linkDir mirrors the directory sfd into dfd (created with perm, plus
// owner access unless root), closes both, and then sets dfd's mode to perm
// if creation did not already give it.
func linkDir(sfd, dfd int, perm os.FileMode) error {
	defer unix.Close(dfd)
	d := os.NewFile(uintptr(sfd), "")
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	for _, ent := range ents {
		name := ent.Name()
		switch t := ent.Type(); {
		case t.IsDir():
			csfd, err := unix.Openat(sfd, name, dirFlags, 0)
			if err != nil {
				return err
			}
			var st unix.Stat_t
			if err := unix.Fstat(csfd, &st); err != nil {
				unix.Close(csfd)
				return err
			}
			cperm := os.FileMode(st.Mode).Perm()
			mk := cperm
			if !isRoot {
				mk |= 0o700
			}
			if err := unix.Mkdirat(dfd, name, uint32(mk)); err != nil {
				unix.Close(csfd)
				return err
			}
			cdfd, err := unix.Openat(dfd, name, dirFlags, 0)
			if err != nil {
				unix.Close(csfd)
				return err
			}
			if err := linkDir(csfd, cdfd, cperm); err != nil {
				return err
			}
		case t&fs.ModeSymlink != 0:
			buf := make([]byte, 256)
			for {
				n, err := unix.Readlinkat(sfd, name, buf)
				if err != nil {
					return err
				}
				if n < len(buf) {
					buf = buf[:n]
					break
				}
				buf = make([]byte, 2*len(buf))
			}
			if err := unix.Symlinkat(string(buf), dfd, name); err != nil {
				return err
			}
		default:
			if err := unix.Linkat(sfd, name, dfd, name, 0); err != nil {
				return err
			}
		}
	}
	made := perm &^ umask // what mkdir gave a directory of this mode
	if !isRoot {
		made = (perm | 0o700) &^ umask
	}
	if made == perm {
		return nil
	}
	return unix.Fchmod(dfd, uint32(perm))
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

// makeRootPrivate runs makePrivate on Root once, before the first mount
// below it rather than at agent start (agent reachability does not wait).
func (s *Server) makeRootPrivate() {
	s.private.Do(func() { s.privateBind = makePrivate(s.cfg.Root) })
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

// flushCache writes the cache filesystem's dirty data back at the end of a
// warm. Otherwise the first start after a warm pays for it: with ext4's
// data=ordered journaling, the next journal commit must write all of it
// first, and metadata operations such as creating the instance's overlay
// directories wait for that commit. On the native backend this flushes the
// host filesystem holding the cache, which the kernel would write back
// within seconds anyway.
func (s *Server) flushCache() {
	f, err := os.Open(s.cfg.Root)
	if err != nil {
		return
	}
	defer f.Close()
	unix.Syncfs(int(f.Fd()))
}

// A viewBuild links a closure's store paths into a view as they become
// available: those already in the cache at once, the others as soon as
// their upload is published. A cold warm thereby builds the view while it
// is still transferring, and its precompose only waits for the remainder.
type viewBuild struct {
	tmp      string
	done     chan struct{} // closed when no layer is pending or in flight
	mu       sync.Mutex
	pending  map[string]bool
	inflight int
	closed   bool
	err      error
}

// viewLinkers bounds concurrent link jobs across all view builds.
var viewLinkers = make(chan struct{}, runtime.NumCPU()*2)

// linkJobs counts the link jobs of view builds that are queued or running:
// the store paths that builds in progress still have to link. A prepared
// build that only waits for uploads has none. The writeback loop waits
// while it is not zero.
var linkJobs atomic.Int32

// prepareView starts building view for layers (biggest first, as
// compose.StoreLayers lists them) unless it exists or is already being
// built.
func (s *Server) prepareView(layers []string, view string) {
	if len(layers) == 0 {
		return
	}
	s.groups.mu.Lock()
	if s.groups.mounted[view] || s.groups.builds[view] != nil {
		s.groups.mu.Unlock()
		return
	}
	if _, err := os.Stat(view + ".done"); err == nil {
		s.groups.mu.Unlock()
		return
	}
	if err := os.MkdirAll(filepath.Dir(view), 0o755); err != nil {
		s.groups.mu.Unlock()
		return
	}
	tmp, err := os.MkdirTemp(filepath.Dir(view), "store.tmp-")
	if err != nil {
		s.groups.mu.Unlock()
		return
	}
	moved := s.stealViews(tmp, layers, view)
	vb := &viewBuild{tmp: tmp, done: make(chan struct{}), pending: map[string]bool{}}
	for _, l := range layers {
		if !moved[filepath.Base(l)] {
			vb.pending[l] = true
		}
	}
	vb.finish() // nothing to link when the donor had it all
	if s.groups.builds == nil {
		s.groups.builds = map[string]*viewBuild{}
	}
	// registered before looking at the cache: a store path published in
	// between is linked either by layerReady or by linkAll
	s.groups.builds[view] = vb
	s.groups.mu.Unlock()
	vb.linkAll(layers)
}

// layerReady links a newly published store path into the views being
// built that need it.
func (s *Server) layerReady(layer string) {
	s.groups.mu.Lock()
	var builds []*viewBuild
	for _, vb := range s.groups.builds {
		builds = append(builds, vb)
	}
	s.groups.mu.Unlock()
	for _, vb := range builds {
		vb.link(layer)
	}
}

// link links layer into the view in the background if it is still
// pending and present.
func (vb *viewBuild) link(layer string) { vb.linkAll([]string{layer}) }

// linkAll links those of layers that are still pending and present into
// the view in the background. The link jobs start in the order of layers.
//
// PERF: callers pass the biggest store paths first. One goroutine takes
// the slots of viewLinkers for the jobs one by one; if every job waited
// for a slot in a goroutine of its own, they would get them in the order
// the scheduler runs those goroutines, and a big store path could be left
// for the end of the build.
func (vb *viewBuild) linkAll(layers []string) {
	jobs := vb.claim(layers)
	if len(jobs) == 0 {
		return
	}
	go func() {
		for _, layer := range jobs {
			viewLinkers <- struct{}{}
			go vb.run(layer)
		}
	}()
}

// claim takes those of layers that are still pending and present off the
// pending set and counts them as link jobs, which the caller runs.
func (vb *viewBuild) claim(layers []string) []string {
	var jobs []string
	vb.mu.Lock()
	for _, layer := range layers {
		if !vb.pending[layer] {
			continue
		}
		if _, err := os.Lstat(layer); err != nil {
			continue // not uploaded yet
		}
		delete(vb.pending, layer)
		jobs = append(jobs, layer)
	}
	vb.inflight += len(jobs)
	linkJobs.Add(int32(len(jobs)))
	vb.mu.Unlock()
	return jobs
}

// run links layer into the view, in a slot of viewLinkers that it frees.
func (vb *viewBuild) run(layer string) {
	err := linkStorePath(layer, vb.tmp)
	<-viewLinkers
	linkJobs.Add(-1)
	vb.mu.Lock()
	if err != nil && vb.err == nil {
		vb.err = err
	}
	vb.inflight--
	vb.finish()
	vb.mu.Unlock()
}

// finish closes done once nothing is pending or in flight (vb.mu held).
func (vb *viewBuild) finish() {
	if !vb.closed && vb.inflight == 0 && len(vb.pending) == 0 {
		vb.closed = true
		close(vb.done)
	}
}

// Background writeback during a warm: while uploads keep arriving, the
// cache filesystem is synced in a loop, so the disk writes the extracted
// data while later artifacts transfer and extract, and the final flush
// only has the tail left. It stops once no upload arrived for a while, and
// it does not sync while a view is being linked.
var (
	writebackMu     sync.Mutex
	writebackActive bool
	writebackLast   time.Time
)

// syncfs flushes the filesystem of fd (a variable for tests).
var syncfs = unix.Syncfs

const writebackIdle = 500 * time.Millisecond

func (s *Server) kickWriteback() {
	writebackMu.Lock()
	defer writebackMu.Unlock()
	writebackLast = time.Now()
	if writebackActive {
		return
	}
	writebackActive = true
	go func() {
		f, err := os.Open(s.cfg.Root)
		if err == nil {
			defer f.Close()
		}
		for {
			// PERF: do not start a sync while a view is being linked. A build
			// is metadata operations only (a link or mkdir per entry), which
			// wait for the journal commit that a sync forces (see
			// flushCache): in local runs a build that overlapped this loop
			// took 29-46% longer (noisy). The loop tries again after its
			// usual 100 ms, and what it skipped is written by the flush that
			// ends the warm, which has more to do when builds ran late
			// (docs/research/perf-round-2-plan.zh-CN.md, C3).
			if err == nil && linkJobs.Load() == 0 {
				syncfs(int(f.Fd()))
			}
			writebackMu.Lock()
			if time.Since(writebackLast) > writebackIdle {
				writebackActive = false
				writebackMu.Unlock()
				return
			}
			writebackMu.Unlock()
			time.Sleep(100 * time.Millisecond)
		}
	}()
}
