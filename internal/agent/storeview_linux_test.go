//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

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
	if s2.privateBind {
		t.Fatal("bound the root twice")
	}
	s.StopAll(context.Background())
	if isMountpoint(dir) || isMountRoot(dir) {
		t.Fatal("root still mounted after StopAll")
	}
}

// fakeBundle creates n store paths (each with one file) and the view
// skeleton of a bundle using them.
func fakeBundle(t testing.TB, s *Server, id string, n int) *compose.Bundle {
	digest := "sha256:" + strings.Repeat(id, 64/len(id))
	b := &compose.Bundle{Digest: digest, StoreView: s.layout.StoreView(digest)}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%032d-p%d", i, i)
		os.MkdirAll(s.layout.StorePath(name), 0o755)
		os.WriteFile(filepath.Join(s.layout.StorePath(name), "f"), []byte(name), 0o644)
		os.MkdirAll(filepath.Join(b.StoreView, name), 0o755)
		b.StorePaths = append(b.StorePaths, name)
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
	p := filepath.Join(b.StoreView, b.StorePaths[7], "f")
	if got, _ := os.ReadFile(p); string(got) != b.StorePaths[7] {
		t.Fatalf("read %q", got)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); !errors.Is(err, unix.EROFS) {
		t.Fatalf("write to the view: %v, want EROFS", err)
	}
	if err := os.WriteFile(filepath.Join(b.StoreView, "new"), nil, 0o644); !errors.Is(err, unix.EROFS) {
		t.Fatalf("create in the view: %v, want EROFS", err)
	}
	// an agent restart rebuilds the view over the old one
	s2, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.ensureStoreView(b); err != nil {
		t.Fatal(err)
	}
	if mps, _ := mountpointsUnder(b.StoreView); len(mps) != 120 {
		t.Fatalf("%d mounts below the view after a rebuild, want 120", len(mps))
	}
	// eviction removes the whole view
	for i := 0; i < maxStoreViews; i++ {
		if err := s.ensureStoreView(fakeBundle(t, s, fmt.Sprintf("%x", i+1), 3)); err != nil {
			t.Fatal(err)
		}
	}
	if isMountpoint(b.StoreView) {
		t.Fatal("evicted view is still mounted")
	}
	if mps, _ := mountpointsUnder(b.StoreView); len(mps) != 0 {
		t.Fatalf("%d mounts left below an evicted view", len(mps))
	}
}

// BenchmarkStoreView measures building a 120-path view and evicting it.
func BenchmarkStoreView(b *testing.B) {
	s := rootServer(b)
	bundles := []*compose.Bundle{fakeBundle(b, s, "a", 120)}
	for i := 0; i < maxStoreViews; i++ {
		bundles = append(bundles, fakeBundle(b, s, fmt.Sprintf("%x", i+1), 120))
	}
	i := 0
	for b.Loop() {
		// cycling through cap+1 bundles: every call builds one view and
		// evicts another
		if err := s.ensureStoreView(bundles[i%len(bundles)]); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

// BenchmarkNew measures agent start on a fresh native root (it binds the
// root onto itself); StopAll is not timed.
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
