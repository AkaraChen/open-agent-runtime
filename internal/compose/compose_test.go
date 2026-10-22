package compose

import (
	"os"
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
	view := false
	for _, m := range r1.Bundle.Spec.Mounts {
		if m.Destination == "/nix/store" {
			view = m.Source == l.StoreView(ClosureKey(p1)) && m.Source == r1.Bundle.StoreView && strings.Contains(strings.Join(m.Options, ","), "ro")
		} else if strings.HasPrefix(m.Destination, "/nix/store/") {
			t.Fatalf("unexpected per-path store mount %+v", m)
		}
	}
	if !view {
		t.Fatal("missing read-only /nix/store view mount")
	}
	if len(r1.Bundle.StoreLayers) != 1 || r1.Bundle.StoreLayers[0] != l.StoreLayer("aaa-git") {
		t.Fatalf("store layers %v", r1.Bundle.StoreLayers)
	}
}

func TestStoreLayersLargestFirst(t *testing.T) {
	pl := &manifest.PlatformLock{StorePaths: []manifest.StorePath{
		{Path: "/nix/store/aaa-small", Size: 10},
		{Path: "/nix/store/bbb-big", Size: 900},
		{Path: "/nix/store/ccc-mid", Size: 50},
		{Path: "/nix/store/ddd-mid", Size: 50},
		{Path: "/nix/store/fff-unsized"},
		{Path: "/nix/store/eee-unsized"},
	}}
	l := Layout{Root: "/cache"}
	var want []string
	for _, n := range []string{"bbb-big", "ccc-mid", "ddd-mid", "aaa-small", "eee-unsized", "fff-unsized"} {
		want = append(want, l.StoreLayer(n))
	}
	if got := StoreLayers(l, pl); !reflect.DeepEqual(got, want) {
		t.Fatalf("store layers\n%v\nwant\n%v", got, want)
	}
	if pl.StorePaths[0].Name() != "aaa-small" {
		t.Fatal("the lock was reordered")
	}
	// the bundle template carries the order to the agent
	pl = lockFor(nil)
	pl.StorePaths = []manifest.StorePath{
		{Path: "/nix/store/aaa-git", Key: "sha256:g", Size: 1},
		{Path: "/nix/store/bbb-node", Key: "sha256:n", Size: 100},
	}
	pl.BundleDigest = pl.ComputeDigest()
	l = warmLayout(t, pl)
	r, err := Compose(l, pl, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{l.StoreLayer("bbb-node"), l.StoreLayer("aaa-git")}; !reflect.DeepEqual(r.Bundle.StoreLayers, want) {
		t.Fatalf("bundle store layers %v, want %v", r.Bundle.StoreLayers, want)
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
	os.RemoveAll(l.StoreLayer("aaa-git"))
	if _, err := Compose(l, pl, "amd64"); err == nil || !strings.Contains(err.Error(), "not warm") {
		t.Fatalf("expected not-warm error, got %v", err)
	}
}
