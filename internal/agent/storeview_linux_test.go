//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
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

func rootServer(t testing.TB) *Server {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir := t.TempDir()
	s, err := New(Config{Root: dir, Mode: "native", Runc: Runc{Root: dir + "/runc"}})
	if err != nil {
		t.Fatal(err)
	}
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
