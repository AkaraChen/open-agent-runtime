//go:build linux

package agent

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
)

func TestUnescapeMountinfo(t *testing.T) {
	for in, want := range map[string]string{
		"/var/lib/oar/x":         "/var/lib/oar/x",
		`/mnt/with\040space`:     "/mnt/with space",
		`/a\011tab\134backslash`: "/a\ttab\\backslash",
	} {
		if got := unescapeMountinfo(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestPrivateRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir := t.TempDir()
	s, err := New(Config{Root: dir, Mode: "native", Runc: Runc{Root: dir + "/runc"}})
	if err != nil {
		t.Fatal(err)
	}
	if isMountRoot(dir) {
		t.Fatal("root bound at agent start; it should wait for the first mount")
	}
	s.makeRootPrivate()
	b, _ := os.ReadFile("/proc/self/mountinfo")
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 6 && f[4] == dir {
			line = l
		}
	}
	if line == "" {
		t.Fatal("root is not a mount point")
	}
	if strings.Contains(line, "shared:") {
		t.Fatalf("root propagation is shared: %s", line)
	}
	// a second agent on the same root does not stack another bind
	s2, err := New(Config{Root: dir, Mode: "native", Runc: Runc{Root: dir + "/runc"}})
	if err != nil {
		t.Fatal(err)
	}
	s2.makeRootPrivate()
	if s2.privateBind {
		t.Fatal("bound the root twice")
	}
	s.StopAll(context.Background())
	if isMountpoint(dir) || isMountRoot(dir) {
		t.Fatal("root still mounted after StopAll")
	}
}

// fakeBundle creates n store paths (each with one file) and a bundle whose
// closure they are.
func fakeBundle(t testing.TB, s *Server, id string, n int) *compose.Bundle {
	b := &compose.Bundle{StoreView: s.layout.StoreView(id)}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%032d-p%d", i, i)
		os.MkdirAll(s.layout.StorePath(name), 0o755)
		os.WriteFile(filepath.Join(s.layout.StorePath(name), "f"), []byte(name), 0o644)
		b.StoreLayers = append(b.StoreLayers, s.layout.StoreLayer(name))
	}
	return b
}

// plainServer is a Server on a temporary root, for tests that do not need
// root.
func plainServer(t testing.TB) *Server {
	dir := t.TempDir()
	s, err := New(Config{Root: dir, Mode: "native", Runc: Runc{Root: dir + "/runc"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rootServer(t testing.TB) *Server {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	s := plainServer(t)
	t.Cleanup(func() { s.StopAll(context.Background()) })
	return s
}

func TestStoreView(t *testing.T) {
	s := rootServer(t)
	b := fakeBundle(t, s, "a", 120)
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(b.StoreLayers[7])
	p := filepath.Join(b.StoreView, name, "f")
	if got, _ := os.ReadFile(p); string(got) != name {
		t.Fatalf("read %q", got)
	}
	// files are hard links to the shared store, not copies
	vi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	si, err := os.Stat(filepath.Join(s.layout.StorePath(name), "f"))
	if err != nil || !os.SameFile(vi, si) {
		t.Fatalf("view file is not a link to the store (%v)", err)
	}
	// the view is complete once and reused, also after an agent restart
	s2, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(b.StoreView); len(entries) != 120 {
		t.Fatalf("%d store paths in the view, want 120", len(entries))
	}
}

// BenchmarkStoreView measures building the hard-link view of a 120-path
// closure (once per closure, at warm time).
func BenchmarkStoreView(b *testing.B) {
	s := rootServer(b)
	layers := fakeBundle(b, s, "x", 120).StoreLayers
	i := 0
	for b.Loop() {
		bundle := &compose.Bundle{StoreLayers: layers, StoreView: s.layout.StoreView(fmt.Sprintf("v%d", i))}
		if err := s.ensureStoreView(bundle); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

// closurePath is a store path of a benchmark closure and the size the lock
// records for it.
type closurePath struct {
	name string
	size int64
}

// skewedClosure creates the store paths of a closure in the cache of s: n
// paths whose entries follow 1/rank, a few big ones among many small ones
// as in a real closure, with names that say nothing about their size. The
// lock records sizes proportional to the entries, which is the best case
// for ordering by size.
func skewedClosure(tb testing.TB, s *Server, n, maxEntries int) []closurePath {
	ranks := rand.New(rand.NewPCG(1, 2)).Perm(n)
	out := make([]closurePath, n)
	for i := range out {
		name := fmt.Sprintf("%032d-p%d", i, i)
		entries := max(1, maxEntries/(ranks[i]+1))
		root := s.layout.StorePath(name)
		for e := 0; e < entries; e++ {
			dir := filepath.Join(root, fmt.Sprintf("d%d", e/16)) // 16 files to a directory
			if e%16 == 0 {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					tb.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", e)), nil, 0o644); err != nil {
				tb.Fatal(err)
			}
		}
		out[i] = closurePath{name, int64(entries) << 10}
	}
	return out
}

// tarClosure extracts the store path tars in dir into the cache of s. The
// size of a store path is that of its tar, as the lock records it.
func tarClosure(tb testing.TB, s *Server, dir string) []closurePath {
	tars, _ := filepath.Glob(filepath.Join(dir, "*.tar"))
	if len(tars) == 0 {
		tb.Skipf("no tar files in %s", dir)
	}
	out := make([]closurePath, len(tars))
	for i, p := range tars {
		name := strings.TrimSuffix(filepath.Base(p), ".tar")
		f, err := os.Open(p)
		if err != nil {
			tb.Fatal(err)
		}
		fi, err := f.Stat()
		if err != nil {
			tb.Fatal(err)
		}
		layer := s.layout.StoreLayer(name)
		if err := os.MkdirAll(layer, 0o755); err != nil {
			tb.Fatal(err)
		}
		_, err = extractTar(f, layer)
		f.Close()
		if err != nil {
			tb.Fatal(err)
		}
		out[i] = closurePath{name, fi.Size()}
	}
	return out
}

// BenchmarkStoreViewOrder measures building the view of a closure whose
// store paths differ a lot in size, with the store paths in an arbitrary
// order ("unordered": eight fixed shuffles, as store path names are
// hashes) and biggest first, as compose.StoreLayers lists them
// ("largest-first"), through the pool of ensureStoreView and as the jobs of
// a prepared build. The closure is synthetic, or the store path tars in
// $OAR_BENCH_TARS (see BenchmarkLinkTree) with the sizes of the tars. The
// synthetic sizes follow the entries, the best case for ordering by size;
// tar sizes predict the link time of a real store path much worse. Each
// build is removed again, untimed; use -benchtime=24x or the like.
func BenchmarkStoreViewOrder(b *testing.B) {
	s := rootServer(b)
	var paths []closurePath
	if dir := os.Getenv("OAR_BENCH_TARS"); dir != "" {
		paths = tarClosure(b, s, dir)
	} else {
		paths = skewedClosure(b, s, 120, 3500)
	}
	lock := &manifest.PlatformLock{}
	for _, p := range paths {
		lock.StorePaths = append(lock.StorePaths, manifest.StorePath{Path: "/nix/store/" + p.name, Size: p.size})
	}
	biggestFirst := compose.StoreLayers(s.layout, lock)
	rng := rand.New(rand.NewPCG(3, 4))
	var shuffles [8][]string
	for i := range shuffles {
		shuffles[i] = slices.Clone(biggestFirst)
		rng.Shuffle(len(shuffles[i]), func(a, c int) { shuffles[i][a], shuffles[i][c] = shuffles[i][c], shuffles[i][a] })
	}
	for _, mode := range []string{"pool", "prepared"} {
		for _, order := range []string{"unordered", "largest-first"} {
			b.Run(mode+"/"+order, func(b *testing.B) {
				for i := 0; b.Loop(); i++ {
					layers := biggestFirst
					if order == "unordered" {
						layers = shuffles[i%len(shuffles)]
					}
					view := s.layout.StoreView(fmt.Sprintf("%s-%s-%d", mode, order, i))
					bundle := &compose.Bundle{StoreLayers: layers, StoreView: view}
					if mode == "prepared" {
						s.prepareView(layers, view)
					}
					if err := s.ensureStoreView(bundle); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					os.RemoveAll(view)
					os.Remove(view + ".done")
					b.StartTimer()
				}
			})
		}
	}
}

// BenchmarkNew measures agent start on a fresh native root; StopAll is
// not timed.
func BenchmarkNew(b *testing.B) {
	if os.Geteuid() != 0 {
		b.Skip("needs root")
	}
	dir := b.TempDir()
	if d := os.Getenv("OAR_BENCH_ROOT"); d != "" {
		dir = d // e.g. under a shared mount, like /var/lib on systemd hosts
	}
	for i := 0; i < b.N; i++ {
		s, err := New(Config{Root: dir, Mode: "native", Runc: Runc{Root: dir + "/runc"}})
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		s.StopAll(context.Background())
		b.StartTimer()
	}
}

func TestPrepareView(t *testing.T) {
	s := rootServer(t)
	b := fakeBundle(t, s, "p", 40)
	// half the store paths "arrive" after the view build started
	late := b.StoreLayers[20:]
	for _, l := range late {
		os.Rename(l, l+".uploading")
	}
	s.prepareView(b.StoreLayers, b.StoreView)
	for _, l := range late {
		os.Rename(l+".uploading", l)
		s.layerReady(l)
	}
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(b.StoreView)
	if len(entries) != 40 {
		t.Fatalf("%d store paths in the view, want 40", len(entries))
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
	// a store path that never arrives: the precompose builds it itself
	b2 := fakeBundle(t, s, "q", 5)
	missing := b2.StoreLayers[4]
	os.Rename(missing, missing+".gone")
	s.prepareView(b2.StoreLayers, b2.StoreView)
	os.Rename(missing+".gone", missing) // present, but layerReady never ran
	if err := s.ensureStoreView(b2); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(b2.StoreView); len(entries) != 5 {
		t.Fatalf("%d store paths in the second view, want 5", len(entries))
	}
}

// waitFor polls ok for up to five seconds.
func waitFor(t testing.TB, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// holdLinks makes link jobs wait for release, after sending the name of
// their store path on started.
func holdLinks(t testing.TB) (started <-chan string, release func()) {
	hold := make(chan struct{})
	ch := make(chan string, 1024)
	orig := linkStorePath
	linkStorePath = func(layer, tmp string) error {
		ch <- filepath.Base(layer)
		<-hold
		return orig(layer, tmp)
	}
	var once sync.Once
	release = func() { once.Do(func() { close(hold) }) }
	t.Cleanup(func() {
		release()
		// jobs that are still running read linkStorePath
		for deadline := time.Now().Add(5 * time.Second); linkJobs.Load() != 0 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		linkStorePath = orig
	})
	return ch, release
}

// stopWriteback ends the writeback loop, if one is running, and waits for it.
func stopWriteback() {
	writebackMu.Lock()
	writebackLast = time.Time{} // idle: the loop looks every 100 ms
	writebackMu.Unlock()
	for {
		writebackMu.Lock()
		active := writebackActive
		writebackMu.Unlock()
		if !active {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countSyncs replaces the sync of the writeback loop with a counter until
// the test ends.
func countSyncs(t testing.TB) *atomic.Int32 {
	stopWriteback() // a loop of an earlier test would use the old syncfs
	n := new(atomic.Int32)
	orig := syncfs
	syncfs = func(int) error { n.Add(1); return nil }
	t.Cleanup(func() {
		stopWriteback()
		syncfs = orig
	})
	return n
}

// While a view is being linked, whether by the pool of ensureStoreView or
// by a prepared build, the writeback loop does not sync; when the build is
// done it does again.
func TestWritebackWaitsForViewBuild(t *testing.T) {
	for _, mode := range []string{"pool", "prepared"} {
		t.Run(mode, func(t *testing.T) {
			s := plainServer(t)
			b := fakeBundle(t, s, mode, 6)
			started, release := holdLinks(t)
			built := make(chan error, 1)
			if mode == "pool" {
				go func() { built <- s.ensureStoreView(b) }()
			} else {
				s.prepareView(b.StoreLayers, b.StoreView)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("no link job started")
			}
			syncs := countSyncs(t)
			s.kickWriteback()
			time.Sleep(250 * time.Millisecond) // two or three rounds of the loop
			if n := syncs.Load(); n != 0 {
				t.Fatalf("%d syncs while a view was being linked", n)
			}
			release()
			if mode == "pool" {
				if err := <-built; err != nil {
					t.Fatal(err)
				}
			} else if err := s.ensureStoreView(b); err != nil { // waits for the build
				t.Fatal(err)
			}
			if n := linkJobs.Load(); n != 0 {
				t.Fatalf("%d link jobs left after the build", n)
			}
			s.kickWriteback()
			waitFor(t, "a sync after the build", func() bool { return syncs.Load() > 0 })
		})
	}
}

// A prepared build that only waits for uploads does not hold the loop back.
func TestWritebackRunsWhileViewWaitsForUploads(t *testing.T) {
	s := plainServer(t)
	b := fakeBundle(t, s, "w", 3)
	for _, l := range b.StoreLayers {
		os.Rename(l, l+".uploading")
	}
	s.prepareView(b.StoreLayers, b.StoreView)
	if n := linkJobs.Load(); n != 0 {
		t.Fatalf("%d link jobs for a view that waits for uploads", n)
	}
	syncs := countSyncs(t)
	s.kickWriteback()
	waitFor(t, "a sync while the view waits for uploads", func() bool { return syncs.Load() > 0 })
	for _, l := range b.StoreLayers {
		os.Rename(l+".uploading", l)
		s.layerReady(l)
	}
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	if n := linkJobs.Load(); n != 0 {
		t.Fatalf("%d link jobs left after the build", n)
	}
}

// A failed link job is not counted for ever, and leaves no complete view.
func TestLinkJobsBalanceOnError(t *testing.T) {
	s := plainServer(t)
	for _, mode := range []string{"pool", "prepared"} {
		b := fakeBundle(t, s, mode, 4)
		os.RemoveAll(filepath.Join(b.StoreLayers[2], "nix")) // nothing to link
		if mode == "prepared" {
			s.prepareView(b.StoreLayers, b.StoreView)
		}
		if err := s.ensureStoreView(b); err == nil {
			t.Fatalf("%s: view built from a broken store path", mode)
		}
		if n := linkJobs.Load(); n != 0 {
			t.Fatalf("%s: %d link jobs left after the failed build", mode, n)
		}
		if _, err := os.Stat(b.StoreView + ".done"); err == nil {
			t.Fatalf("%s: failed view marked complete", mode)
		}
	}
}

// The link jobs of a prepared build start in the order of its layers, both
// those present when it starts and those found when it is completed.
func TestPrepareViewLinksInOrder(t *testing.T) {
	s := plainServer(t)
	b := fakeBundle(t, s, "o", 12)
	slices.Reverse(b.StoreLayers) // not the name order
	// with one slot the jobs run one by one, in the order they got it
	slots, link := viewLinkers, linkStorePath
	viewLinkers = make(chan struct{}, 1)
	var mu sync.Mutex
	var started []string
	linkStorePath = func(layer, tmp string) error {
		mu.Lock()
		started = append(started, layer)
		mu.Unlock()
		return link(layer, tmp)
	}
	t.Cleanup(func() { viewLinkers, linkStorePath = slots, link })
	// the last four are not there when the build starts, and arrive
	// without layerReady: ensureStoreView finds them
	late := b.StoreLayers[8:]
	for _, l := range late {
		os.Rename(l, l+".uploading")
	}
	s.prepareView(b.StoreLayers, b.StoreView)
	waitFor(t, "the first link jobs", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) == 8
	})
	for _, l := range late {
		os.Rename(l+".uploading", l)
	}
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(started, b.StoreLayers) {
		base := func(layers []string) (out []string) {
			for _, l := range layers {
				out = append(out, filepath.Base(l))
			}
			return
		}
		t.Fatalf("link jobs started in the order\n%v\nwant\n%v", base(started), base(b.StoreLayers))
	}
}

func TestLinkTree(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(filepath.Join(src, "bin", "deep"), 0o755)
	os.MkdirAll(filepath.Join(src, "ro", "inner"), 0o755)
	os.WriteFile(filepath.Join(src, "bin", "tool"), []byte("t"), 0o555)
	os.WriteFile(filepath.Join(src, "ro", "inner", "g"), []byte("g"), 0o444)
	os.Symlink("../bin/tool", filepath.Join(src, "ro", "link"))
	long := strings.Repeat("l", 700)
	os.Symlink(long, filepath.Join(src, "longlink"))
	os.Chmod(filepath.Join(src, "bin"), 0o755) // whatever the umask
	os.Chmod(filepath.Join(src, "bin", "deep"), 0o755)
	os.Chmod(filepath.Join(src, "ro", "inner"), 0o555)
	os.Chmod(filepath.Join(src, "ro"), 0o555)
	os.Chmod(src, 0o555)
	t.Cleanup(func() {
		for _, root := range []string{src, dst} {
			os.Chmod(root, 0o755)
			os.Chmod(filepath.Join(root, "ro"), 0o755)
			os.Chmod(filepath.Join(root, "ro", "inner"), 0o755)
		}
	})
	if err := linkTree(src, dst); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{
		"": 0o555, "bin": 0o755, "bin/deep": 0o755, "ro": 0o555, "ro/inner": 0o555,
	} {
		st, err := os.Stat(filepath.Join(dst, p))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%q: %v %v, want %v", p, st, err, want)
		}
	}
	for _, p := range []string{"bin/tool", "ro/inner/g"} {
		a, _ := os.Stat(filepath.Join(src, p))
		b, err := os.Stat(filepath.Join(dst, p))
		if err != nil || !os.SameFile(a, b) {
			t.Errorf("%s is not a hard link (%v)", p, err)
		}
	}
	if l, _ := os.Readlink(filepath.Join(dst, "ro", "link")); l != "../bin/tool" {
		t.Errorf("link %q", l)
	}
	if l, _ := os.Readlink(filepath.Join(dst, "longlink")); l != long {
		t.Errorf("long link has %d bytes", len(l))
	}
}
