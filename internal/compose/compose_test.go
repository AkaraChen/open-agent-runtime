package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
)

func lockFor(comps []manifest.Component) *manifest.PlatformLock {
	pl := &manifest.PlatformLock{
		Platform:   "linux/amd64",
		Base:       manifest.Base{Ref: "debian", Digest: "sha256:d", Key: "sha256:base"},
		StorePaths: []manifest.StorePath{{Path: "/nix/store/aaa-git", Key: "sha256:s"}},
		Components: comps,
		Process:    manifest.Process{Args: []string{"/opt/a"}, Env: []string{"PATH=/bin"}, Cwd: "/"},
		Hostname:   "oar",
	}
	pl.BundleDigest = pl.ComputeDigest()
	return pl
}

func warmLayout(t *testing.T, pl *manifest.PlatformLock) Layout {
	l := Layout{Root: t.TempDir()}
	dirs := []string{l.BaseDir(pl.Base.Key)}
	for _, sp := range pl.StorePaths {
		dirs = append(dirs, l.StorePath(sp.Name()))
	}
	for _, c := range pl.Components {
		dirs = append(dirs, l.ComponentDir(c.Key))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func TestComposeDeterministicAndCached(t *testing.T) {
	a := manifest.Component{Name: "a", Kind: "binary", Key: "sha256:ca", Files: []string{"opt/a"}}
	b := manifest.Component{Name: "b", Kind: "file", Key: "sha256:cb", Files: []string{"etc/b"}}
	p1 := lockFor([]manifest.Component{a, b})
	p2 := lockFor([]manifest.Component{b, a})
	if p1.BundleDigest != p2.BundleDigest {
		t.Fatal("digest depends on component order")
	}
	l := warmLayout(t, p1)
	r1, err := Compose(l, p1, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Cached {
		t.Fatal("first compose should not be cached")
	}
	r2, err := Compose(l, p2, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Cached || !reflect.DeepEqual(r1.Bundle.Layers, r2.Bundle.Layers) {
		t.Fatalf("second compose: cached=%v layers %v vs %v", r2.Cached, r1.Bundle.Layers, r2.Bundle.Layers)
	}
	if last := r1.Bundle.Layers[len(r1.Bundle.Layers)-1]; last != l.BaseDir(p1.Base.Key) {
		t.Fatalf("base must be the lowest layer, got %s", last)
	}
	var view bool
	for _, m := range r1.Bundle.Spec.Mounts {
		if strings.HasPrefix(m.Destination, "/nix/store/") || m.Source == l.StoreRoot() {
			t.Fatalf("unexpected store mount %+v", m)
		}
		if m.Destination == "/nix/store" {
			view = m.Source == l.StoreView(p1.BundleDigest) && strings.Contains(strings.Join(m.Options, ","), "ro")
		}
	}
	if !view || r1.Bundle.StoreView != l.StoreView(p1.BundleDigest) || len(r1.Bundle.StorePaths) != 1 || r1.Bundle.StorePaths[0] != "aaa-git" {
		t.Fatalf("store view: mount=%v bundle=%+v", view, r1.Bundle)
	}
	entries, err := os.ReadDir(l.StoreView(p1.BundleDigest))
	if err != nil || len(entries) != 1 || entries[0].Name() != "aaa-git" {
		t.Fatalf("store skeleton: %v %v", entries, err)
	}
}

func TestComposeRejects(t *testing.T) {
	pl := lockFor(nil)
	l := warmLayout(t, pl)
	if _, err := Compose(l, pl, "arm64"); err == nil {
		t.Fatal("expected arch mismatch")
	}
	tampered := *pl
	tampered.Process.Args = []string{"/bin/evil"}
	if _, err := Compose(l, &tampered, "amd64"); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest mismatch, got %v", err)
	}
	os.RemoveAll(filepath.Join(l.StoreRoot(), "aaa-git"))
	if _, err := Compose(l, pl, "amd64"); err == nil || !strings.Contains(err.Error(), "not warm") {
		t.Fatalf("expected not-warm error, got %v", err)
	}
}
