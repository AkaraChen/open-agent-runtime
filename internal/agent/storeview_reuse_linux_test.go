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

// waitViewBuilt waits until a donor's background rebuild has published the
// view, then checks it.
func waitViewBuilt(t testing.TB, s *Server, b *compose.Bundle) {
	t.Helper()
	for range 500 {
		s.groups.mu.Lock()
		building := s.groups.builds[b.StoreView] != nil
		s.groups.mu.Unlock()
		if _, err := os.Stat(b.StoreView + ".done"); err == nil && !building {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	checkView(t, b)
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
	// a is whole again in the background, its store paths linked back
	waitViewBuilt(t, s, a)
	if got := dirIno(t, filepath.Join(a.StoreView, name)); got == ino {
		t.Fatal("donor got the moved directory back")
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
		// the instance goes away: the next closure may take from a or b
		os.RemoveAll(dir)
		c := fakeClosure(t, s, "c", 2, 42)
		if err := s.ensureStoreView(c); err != nil {
			t.Fatal(err)
		}
		checkView(t, c)
		waitViewBuilt(t, s, a)
		waitViewBuilt(t, s, b)
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
	// rolling back at once takes over the donor's rebuild
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
	waitViewBuilt(t, s, b)
}

// A restart in the middle of a donor's rebuild leaves a directory without
// its marker: the next request for it builds it from scratch.
func TestViewReuseInterrupted(t *testing.T) {
	s := plainServer(t)
	a := fakeClosure(t, s, "a", 0, 40)
	// a donor as the agent left it when it died: no marker, a few store paths
	os.MkdirAll(filepath.Join(a.StoreView, filepath.Base(a.StoreLayers[0])), 0o755)
	if err := s.ensureStoreView(a); err != nil {
		t.Fatal(err)
	}
	checkView(t, a)
}
