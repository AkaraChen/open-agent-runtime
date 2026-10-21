//go:build linux

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		os.WriteFile(filepath.Join(s.layout.StorePath(name), "f"), []byte(name), 0o644)
		b.StoreLayers = append(b.StoreLayers, s.layout.StoreLayer(name))
	}
	return b
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

func TestViewReuse(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(a.StoreLayers[3])
	ino := dirIno(t, filepath.Join(a.StoreView, name))

	// one path added, one dropped: the new view takes the rest from a
	b := fakeClosure(t, s, "b", 1, 41)
	if err := s.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	checkView(t, b)
	if got := dirIno(t, filepath.Join(b.StoreView, name)); got != ino {
		t.Fatalf("store path was linked again (inode %d, was %d), not moved", got, ino)
	}
	// a is invalidated, and gone once the cleanup has run
	s.cleanups.Wait()
	if _, err := os.Stat(a.StoreView + ".done"); err == nil {
		t.Fatal("donor still marked complete")
	}
	if _, err := os.Stat(a.StoreView); err == nil {
		t.Fatal("donor still there")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(a.StoreView), "store.tmp-*")); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
	// b is running: rolling back to a builds it from scratch and leaves b whole
	os.MkdirAll(s.instDir("svc-1"), 0o755)
	os.WriteFile(filepath.Join(s.instDir("svc-1"), viewMarker), []byte(b.StoreView), 0o644)
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	checkView(t, b)
	if dirIno(t, filepath.Join(a.StoreView, name)) == ino {
		t.Fatal("rebuilt view has the moved directory")
	}
	// b is not running: a takes from it in turn, and b is the one to rebuild
	os.RemoveAll(s.instDir("svc-1"))
	c := fakeClosure(t, s, "c", 0, 41)
	if err := s.ensureStoreView(c); err != nil {
		t.Fatal(err)
	}
	checkView(t, c)
	s.cleanups.Wait()
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
		if err := s.ensureStoreView(a); err != nil {
			t.Fatal(err)
		}
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
	}
}

func TestViewReuseNeedsSimilarity(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
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
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
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
	// rolling back is a plain build
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
}

// A donor the agent left without its marker (killed after the steal) is
// built from scratch by the next request for it.
func TestViewReuseInterrupted(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	os.MkdirAll(filepath.Join(a.StoreView, filepath.Base(a.StoreLayers[0])), 0o755)
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
}

// BenchmarkViewReuse times the view of a closure that differs from an
// existing, unused one by one store path (120 paths of ~190 files): built
// by linking everything, or by taking from the donor; and then the donor's
// own view again, which a rollback pays for in full while the new closure runs.
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
				if err := s.ensureStoreView(a); err != nil {
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
