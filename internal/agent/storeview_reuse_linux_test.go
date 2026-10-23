//go:build linux

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
)

// fakeClosure is fakeBundle for the store paths numbered from..to-1, so
// closures can overlap.
func fakeClosure(t testing.TB, s *Server, id string, from, to int) *compose.Bundle {
	b := &compose.Bundle{StoreView: s.layout.StoreView(id)}
	for i := from; i < to; i++ {
		name := fmt.Sprintf("%032d-p%d", i, i)
		os.MkdirAll(s.layout.StorePath(name), 0o755)
		// a store path that exists stays as it is: views hold links to its files
		if f := filepath.Join(s.layout.StorePath(name), "f"); !fileExists(f) {
			os.WriteFile(f, []byte(name), 0o644)
		}
		b.StoreLayers = append(b.StoreLayers, s.layout.StoreLayer(name))
	}
	return b
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// plainServer needs no root: views are only directories and hard links.
func plainServer(t testing.TB) *Server {
	s, err := New(Config{Root: t.TempDir(), Mode: "native", Runc: Runc{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dirIno(t testing.TB, path string) uint64 {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

func checkView(t testing.TB, b *compose.Bundle) {
	t.Helper()
	entries, _ := os.ReadDir(b.StoreView)
	if len(entries) != len(b.StoreLayers) {
		t.Fatalf("%d store paths in %s, want %d", len(entries), b.StoreView, len(b.StoreLayers))
	}
	for _, l := range b.StoreLayers {
		name := filepath.Base(l)
		if got, _ := os.ReadFile(filepath.Join(b.StoreView, name, "f")); string(got) != name {
			t.Fatalf("%s: read %q", name, got)
		}
	}
	if _, err := os.Stat(b.StoreView + ".done"); err != nil {
		t.Fatal("view not marked complete")
	}
}

// startedAndIdle builds the bundle's view and marks it used, as an instance
// that has been created on it and is gone again would have left it.
func startedAndIdle(t testing.TB, s *Server, b *compose.Bundle) {
	t.Helper()
	if err := s.useStoreView(b); err != nil {
		t.Fatal(err)
	}
}

// mountedBy makes it look as if the instance id mounted the bundle's view.
func mountedBy(t testing.TB, s *Server, id string, b *compose.Bundle) (release func()) {
	t.Helper()
	dir := s.instDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, viewMarker), []byte(b.StoreView), 0o644); err != nil {
		t.Fatal(err)
	}
	return func() { os.RemoveAll(dir) }
}

func TestViewReuse(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	startedAndIdle(t, s, a)
	name := filepath.Base(a.StoreLayers[3])
	ino := dirIno(t, filepath.Join(a.StoreView, name))

	// one path added, one dropped: the new view takes the rest from a
	b := fakeClosure(t, s, "b", 1, 41)
	startedAndIdle(t, s, b)
	checkView(t, b)
	if got := dirIno(t, filepath.Join(b.StoreView, name)); got != ino {
		t.Fatalf("store path was linked again (inode %d, was %d), not moved", got, ino)
	}
	// a is invalidated, and gone once the cleanup has run
	s.cleanups.Wait()
	if _, err := os.Stat(a.StoreView + ".done"); err == nil {
		t.Fatal("donor still marked complete")
	}
	if _, err := os.Stat(a.StoreView + ".used"); err == nil {
		t.Fatal("donor still marked used")
	}
	if _, err := os.Stat(a.StoreView); err == nil {
		t.Fatal("donor still there")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(a.StoreView), "store.tmp-*")); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
	// b is running: rolling back to a builds it from scratch and leaves b whole
	release := mountedBy(t, s, "svc-1", b)
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	checkView(t, b)
	if dirIno(t, filepath.Join(a.StoreView, name)) == ino {
		t.Fatal("rebuilt view has the moved directory")
	}
	// b is not running any more and has been started before; a has only been
	// built again, as a warm would: c takes from b, and b is the one to rebuild
	release()
	c := fakeClosure(t, s, "c", 0, 41)
	if err := s.ensureStoreView(c); err != nil {
		t.Fatal(err)
	}
	checkView(t, c)
	checkView(t, a)
	s.cleanups.Wait()
	if _, err := os.Stat(b.StoreView + ".done"); err == nil {
		t.Fatal("b was not taken apart")
	}
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	checkView(t, b)
}

func TestViewReuseSkipsViewsInUse(t *testing.T) {
	for _, old := range []bool{false, true} {
		s := plainServer(t)
		a := fakeClosure(t, s, "a", 0, 40)
		a.Digest = "sha256:aaaa"
		startedAndIdle(t, s, a)
		// an instance mounting a: a marker, or (older agent) its bundle's view
		dir := s.instDir("svc-1")
		os.MkdirAll(dir, 0o755)
		if old {
			data, _ := json.Marshal(a)
			os.MkdirAll(s.layout.BundleDir(a.Digest), 0o755)
			os.WriteFile(filepath.Join(s.layout.BundleDir(a.Digest), "bundle.json"), data, 0o644)
			os.WriteFile(filepath.Join(dir, "instance.json"), []byte(`{"name":"svc","id":"svc-1","bundleDigest":"`+a.Digest+`"}`), 0o644)
		} else {
			os.WriteFile(filepath.Join(dir, viewMarker), []byte(a.StoreView), 0o644)
		}
		name := filepath.Base(a.StoreLayers[3])
		ino := dirIno(t, filepath.Join(a.StoreView, name))
		b := fakeClosure(t, s, "b", 1, 41)
		if err := s.ensureStoreView(b); err != nil {
			t.Fatal(err)
		}
		checkView(t, b)
		checkView(t, a)
		if dirIno(t, filepath.Join(a.StoreView, name)) != ino || dirIno(t, filepath.Join(b.StoreView, name)) == ino {
			t.Fatalf("old=%v: a view in use was taken apart", old)
		}
		// the instance goes away: the next closure may take from a
		os.RemoveAll(dir)
		c := fakeClosure(t, s, "c", 2, 42)
		if err := s.ensureStoreView(c); err != nil {
			t.Fatal(err)
		}
		checkView(t, c)
		s.cleanups.Wait()
		if _, err := os.Stat(a.StoreView + ".done"); err == nil {
			t.Fatalf("old=%v: a, idle now, was not taken", old)
		}
	}
}

func TestViewReuseNeedsSimilarity(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	startedAndIdle(t, s, a)
	name := filepath.Base(a.StoreLayers[30])
	ino := dirIno(t, filepath.Join(a.StoreView, name))
	// shares 10 of 40: linking is as cheap as the damage to a
	c := fakeClosure(t, s, "c", 30, 70)
	if err := s.ensureStoreView(c); err != nil {
		t.Fatal(err)
	}
	checkView(t, c)
	if dirIno(t, filepath.Join(a.StoreView, name)) != ino {
		t.Fatal("took from a dissimilar view")
	}
}

func TestViewReuseWhilePreparing(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	startedAndIdle(t, s, a)
	// the warm announces the closure first, its new path arrives later
	b := fakeClosure(t, s, "b", 1, 41)
	late := b.StoreLayers[39]
	os.Rename(late, late+".uploading")
	s.prepareView(b.StoreLayers, b.StoreView)
	os.Rename(late+".uploading", late)
	s.layerReady(late)
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	checkView(t, b)
	s.cleanups.Wait()
	if _, err := os.Stat(a.StoreView + ".done"); err == nil || s.groups.steals.Load() != 1 {
		t.Fatal("the warm did not take from a")
	}
	// rolling back is a plain build, here by another warm
	s.prepareView(a.StoreLayers, a.StoreView)
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	if s.groups.steals.Load() != 1 || s.groups.rebuilds.Load() != 1 {
		t.Fatalf("%d steals, %d rebuilds", s.groups.steals.Load(), s.groups.rebuilds.Load())
	}
}

// A donor the agent left without its marker (killed after the steal) is
// built from scratch by the next request for it, and the mark it may still
// have does not carry over to the view that is built.
func TestViewReuseInterrupted(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	os.MkdirAll(filepath.Join(a.StoreView, filepath.Base(a.StoreLayers[0])), 0o755)
	os.WriteFile(a.StoreView+".used", nil, 0o644)
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	if _, err := os.Stat(a.StoreView + ".used"); err == nil {
		t.Fatal("the view built carries the mark of the one that was taken apart")
	}
	b := fakeClosure(t, s, "b", 1, 41)
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	if s.groups.steals.Load() != 0 {
		t.Fatal("took from a view that was never started")
	}
}

// A view that was warmed ahead and never started is not a donor, however
// well it matches: the first start of its bundle expects to find it whole.
// Once an instance has been created on it, it is one.
func TestViewReuseNeedsStarted(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	if err := s.ensureStoreView(a); err != nil { // a warm
		t.Fatal(err)
	}
	name := filepath.Base(a.StoreLayers[3])
	ino := dirIno(t, filepath.Join(a.StoreView, name))
	b := fakeClosure(t, s, "b", 1, 41)
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	checkView(t, b)
	if dirIno(t, filepath.Join(b.StoreView, name)) == ino || s.groups.steals.Load() != 0 {
		t.Fatal("took from a view that was never started")
	}

	// the first start of a's bundle
	startedAndIdle(t, s, a)
	c := fakeClosure(t, s, "c", 2, 42)
	if err := s.ensureStoreView(c); err != nil {
		t.Fatal(err)
	}
	checkView(t, c)
	if dirIno(t, filepath.Join(c.StoreView, name)) != ino || s.groups.steals.Load() != 1 {
		t.Fatalf("a, started and idle, was not taken from (%d steals)", s.groups.steals.Load())
	}
	s.cleanups.Wait()
}

// The benchmark's setup: every combination is warmed, by a build of its own
// or while its artifacts upload, before any is started. None of the views is
// taken apart for another, whichever order they are started in.
func TestViewReuseKeepsWarmedViews(t *testing.T) {
	s := plainServer(t)
	var warmed []*compose.Bundle
	for i := 0; i < 5; i++ {
		b := fakeClosure(t, s, fmt.Sprintf("w%d", i), i, 40+i)
		if i%2 == 1 {
			s.prepareView(b.StoreLayers, b.StoreView)
		}
		if err := s.ensureStoreView(b); err != nil {
			t.Fatal(err)
		}
		warmed = append(warmed, b)
	}
	for i := len(warmed) - 1; i >= 0; i-- {
		startedAndIdle(t, s, warmed[i])
	}
	for _, b := range warmed {
		checkView(t, b)
	}
	if s.groups.steals.Load() != 0 || s.groups.rebuilds.Load() != 0 {
		t.Fatalf("%d steals, %d rebuilds while starting warmed views", s.groups.steals.Load(), s.groups.rebuilds.Load())
	}
}

// The mark belongs to the closure and outlives the agent.
func TestViewReuseMarkPersists(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	// a bundle with another digest, but the same closure and so the same view
	other := *a
	other.Digest = "sha256:other"
	startedAndIdle(t, s, &other)
	if _, err := os.Stat(a.StoreView + ".used"); err != nil {
		t.Fatal("view not marked used")
	}
	name := filepath.Base(a.StoreLayers[3])
	ino := dirIno(t, filepath.Join(a.StoreView, name))

	s2, err := New(s.cfg) // the agent restarts
	if err != nil {
		t.Fatal(err)
	}
	c := fakeClosure(t, s2, "c", 2, 42)
	if err := s2.ensureStoreView(c); err != nil {
		t.Fatal(err)
	}
	checkView(t, c)
	if dirIno(t, filepath.Join(c.StoreView, name)) != ino || s2.groups.steals.Load() != 1 {
		t.Fatal("the restarted agent did not take from the view that had been started")
	}
	s2.cleanups.Wait()
}

// A view that was taken apart and is built again, as a warm does, has not
// been started: it is warmed ahead again, and no donor until an instance is
// created on it. The counters tell the two events apart.
func TestViewReuseTakenViewIsWarmedAgain(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	startedAndIdle(t, s, a)
	b := fakeClosure(t, s, "b", 1, 41)
	if err := s.ensureStoreView(b); err != nil { // takes from a
		t.Fatal(err)
	}
	s.cleanups.Wait()
	if s.groups.steals.Load() != 1 || s.groups.rebuilds.Load() != 0 {
		t.Fatalf("%d steals, %d rebuilds after the first steal", s.groups.steals.Load(), s.groups.rebuilds.Load())
	}
	if err := s.ensureStoreView(a); err != nil { // warmed again
		t.Fatal(err)
	}
	checkView(t, a)
	if s.groups.steals.Load() != 1 || s.groups.rebuilds.Load() != 1 {
		t.Fatalf("%d steals, %d rebuilds after a was built again", s.groups.steals.Load(), s.groups.rebuilds.Load())
	}
	if _, err := os.Stat(a.StoreView + ".used"); err == nil {
		t.Fatal("the view built again is marked used")
	}

	// neither a (warmed again) nor b (built by a steal, not started) is a donor
	c := fakeClosure(t, s, "c", 2, 42)
	if err := s.ensureStoreView(c); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	checkView(t, b)
	checkView(t, c)
	if s.groups.steals.Load() != 1 {
		t.Fatal("took from a view that was not started")
	}
	// the first start of a's bundle makes it one
	startedAndIdle(t, s, a)
	d := fakeClosure(t, s, "d", 3, 43)
	if err := s.ensureStoreView(d); err != nil {
		t.Fatal(err)
	}
	s.cleanups.Wait()
	checkView(t, d)
	if _, err := os.Stat(a.StoreView + ".done"); err == nil || s.groups.steals.Load() != 2 || s.groups.rebuilds.Load() != 1 {
		t.Fatalf("a was not taken from: %d steals, %d rebuilds", s.groups.steals.Load(), s.groups.rebuilds.Load())
	}
}

// The agent's stats carry the counters.
func TestStatsViewCounters(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	startedAndIdle(t, s, a)
	if err := s.ensureStoreView(fakeClosure(t, s, "b", 1, 41)); err != nil {
		t.Fatal(err)
	}
	s.cleanups.Wait()
	if st := s.stats(); st.ViewSteals != 1 || st.ViewRebuilds != 0 {
		t.Fatalf("stats: %d steals, %d rebuilds", st.ViewSteals, st.ViewRebuilds)
	}
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	if st := s.stats(); st.ViewSteals != 1 || st.ViewRebuilds != 1 {
		t.Fatalf("stats: %d steals, %d rebuilds", st.ViewSteals, st.ViewRebuilds)
	}
}

// A view that createInstance holds is not taken, however idle it looks: the
// instance's marker, which protects the view from then on, is written only
// after the first request for the view.
func TestViewReuseSkipsHeldViews(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	startedAndIdle(t, s, a)
	name := filepath.Base(a.StoreLayers[3])
	ino := dirIno(t, filepath.Join(a.StoreView, name))

	first, second := s.holdView(a.StoreView), s.holdView(a.StoreView)
	b := fakeClosure(t, s, "b", 1, 41)
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	first() // one hold is left
	c := fakeClosure(t, s, "c", 2, 42)
	if err := s.ensureStoreView(c); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	if dirIno(t, filepath.Join(a.StoreView, name)) != ino || s.groups.steals.Load() != 0 {
		t.Fatal("took a view that was held")
	}
	second()
	d := fakeClosure(t, s, "d", 3, 43)
	if err := s.ensureStoreView(d); err != nil {
		t.Fatal(err)
	}
	s.cleanups.Wait()
	if _, err := os.Stat(a.StoreView + ".done"); err == nil || s.groups.steals.Load() != 1 {
		t.Fatal("did not take the view once it was released")
	}
}

// The instance's marker comes before its request for the view, and a request
// for another closure picks donors under the lock that the request holds:
// however the two interleave, the view is rebuilt before the instance gets
// it, or the steal sees the marker. Run with -race.
func TestViewReuseConcurrentUse(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	startedAndIdle(t, s, a)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // closures that would like to take a apart
		defer wg.Done()
		for i := 0; i < 60; i++ {
			c := fakeClosure(t, s, fmt.Sprintf("c%d", i), 1+i%3, 41+i%3)
			if err := s.ensureStoreView(c); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 60; i++ {
		var rebuilt int64
		var unhold func()
		if i%2 == 1 {
			// createInstance: a first request, then the instance directory
			// with its marker, then the request that marks the view
			unhold = s.holdView(a.StoreView)
			if err := s.ensureStoreView(a); err != nil {
				t.Fatal(err)
			}
			rebuilt = s.groups.rebuilds.Load()
			runtime.Gosched()
			runtime.Gosched()
		}
		release := mountedBy(t, s, "svc-1", a)
		if err := s.useStoreView(a); err != nil {
			t.Fatal(err)
		}
		if unhold != nil {
			unhold()
			if s.groups.rebuilds.Load() != rebuilt {
				t.Fatal("the view was taken between the first request and the marker")
			}
		}
		for j := 0; j < 5; j++ {
			runtime.Gosched()
			checkView(t, a) // whole for as long as the instance is there
		}
		release()
	}
	wg.Wait()
	s.cleanups.Wait()
}

// An instance marks the view it is created on, and holds it while it exists.
func TestInstantiateMarksView(t *testing.T) {
	s := rootServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	a.Layers = []string{t.TempDir()}
	a.Spec = &specs.Spec{Process: &specs.Process{}, Linux: &specs.Linux{}}
	in, err := s.instantiate("svc", a, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gone := false
	remove := func() {
		if !gone {
			unix.Unmount(filepath.Join(in.dir, "rootfs"), unix.MNT_DETACH)
			os.RemoveAll(in.dir)
			gone = true
		}
	}
	t.Cleanup(remove)
	if _, err := os.Stat(a.StoreView + ".used"); err != nil {
		t.Fatal("the view of a new instance is not marked used")
	}
	// a similar closure does not take the view the instance has
	b := fakeClosure(t, s, "b", 1, 41)
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	// the instance is gone: the view has been started and is idle
	remove()
	c := fakeClosure(t, s, "c", 2, 42)
	if err := s.ensureStoreView(c); err != nil {
		t.Fatal(err)
	}
	s.cleanups.Wait()
	if _, err := os.Stat(a.StoreView + ".done"); err == nil {
		t.Fatal("the view of a gone instance was not taken")
	}
}

// BenchmarkViewReuse times the view of a closure that differs by one store
// path (120 paths of ~190 files) from an existing view that has been started
// and is idle: built by linking everything (reuse=false: an instance has the
// existing view), or by taking from the donor; and then the donor's own view
// again, which a rollback pays for in full while the new closure runs.
func BenchmarkViewReuse(b *testing.B) {
	mk := func(s *Server, id string, from, to int) *compose.Bundle {
		bundle := &compose.Bundle{StoreView: s.layout.StoreView(id)}
		for i := from; i < to; i++ {
			name := fmt.Sprintf("%032d-p%d", i, i)
			dir := filepath.Join(s.layout.StorePath(name), "lib")
			if _, err := os.Stat(dir); err != nil {
				for d := 0; d < 10; d++ {
					os.MkdirAll(filepath.Join(dir, fmt.Sprint(d)), 0o755)
					for f := 0; f < 19; f++ {
						os.WriteFile(filepath.Join(dir, fmt.Sprint(d), fmt.Sprint(f)), []byte(name), 0o644)
					}
				}
				os.WriteFile(filepath.Join(s.layout.StorePath(name), "f"), []byte(name), 0o644)
			}
			bundle.StoreLayers = append(bundle.StoreLayers, s.layout.StoreLayer(name))
		}
		return bundle
	}
	for _, reuse := range []bool{false, true} {
		b.Run(fmt.Sprintf("reuse=%v", reuse), func(b *testing.B) {
			s := plainServer(b)
			var build, rollback time.Duration
			for i := 0; i < b.N; i++ {
				a := mk(s, fmt.Sprintf("a%d", i), 0, 120)
				if err := s.useStoreView(a); err != nil {
					b.Fatal(err)
				}
				nb := mk(s, fmt.Sprintf("b%d", i), 1, 121)
				if !reuse {
					// as if a were in use
					os.MkdirAll(s.instDir("x"), 0o755)
					os.WriteFile(filepath.Join(s.instDir("x"), viewMarker), []byte(a.StoreView), 0o644)
				}
				t := time.Now()
				if err := s.ensureStoreView(nb); err != nil {
					b.Fatal(err)
				}
				build += time.Since(t)
				s.cleanups.Wait()
				if reuse {
					// rolling back while the new closure runs: nothing to take from
					os.MkdirAll(s.instDir("x"), 0o755)
					os.WriteFile(filepath.Join(s.instDir("x"), viewMarker), []byte(nb.StoreView), 0o644)
					t = time.Now()
					if err := s.ensureStoreView(a); err != nil {
						b.Fatal(err)
					}
					rollback += time.Since(t)
				}
				os.RemoveAll(s.instDir("x"))
				for _, v := range []string{a.StoreView, nb.StoreView} {
					os.RemoveAll(v)
					os.RemoveAll(v + ".done")
				}
			}
			n := time.Duration(b.N)
			b.ReportMetric(float64((build/n).Microseconds())/1000, "new-view-ms")
			b.ReportMetric(float64((rollback/n).Microseconds())/1000, "rollback-ms")
		})
	}
}
