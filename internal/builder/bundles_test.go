package builder

import (
	"strings"
	"testing"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

func writeBundle(t *testing.T, art string, args ...string) *manifest.PlatformLock {
	pl := &manifest.PlatformLock{Platform: "linux/amd64", Base: manifest.Base{Digest: "d", Key: "k"},
		Process: manifest.Process{Args: args}}
	pl.BundleDigest = pl.ComputeDigest()
	if err := manifest.WriteJSON(BundlePath(art, pl.BundleDigest), pl); err != nil {
		t.Fatal(err)
	}
	return pl
}

func TestFindBundle(t *testing.T) {
	art := t.TempDir()
	a := writeBundle(t, art, "/a")
	b := writeBundle(t, art, "/b")
	for _, ref := range []string{a.BundleDigest, strings.TrimPrefix(a.BundleDigest, "sha256:")[:12]} {
		got, err := FindBundle(art, ref)
		if err != nil || got.BundleDigest != a.BundleDigest {
			t.Fatalf("%s: %v %v", ref, got, err)
		}
	}
	if _, err := FindBundle(art, "abc"); err == nil {
		t.Fatal("short reference accepted")
	}
	if _, err := FindBundle(art, "ffffffffffff"); err == nil {
		t.Fatal("missing bundle found")
	}
	// tampering is detected
	b.Process.Args = []string{"/evil"}
	manifest.WriteJSON(BundlePath(art, b.BundleDigest), b)
	if _, err := FindBundle(art, b.BundleDigest); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corrupt error, got %v", err)
	}
}

func TestBundleForConfig(t *testing.T) {
	art := t.TempDir()
	a := writeBundle(t, art, "/a")
	cfgA := &spec.Config{Hash: "sha256:aaaa", Runtime: spec.Runtime{Platform: "linux/amd64"}}
	cfgB := &spec.Config{Hash: "sha256:bbbb", Runtime: spec.Runtime{Platform: "linux/amd64"}}
	lock := &manifest.Lock{Platforms: map[string]*manifest.PlatformLock{"linux/amd64": a}}
	if err := recordConfig(art, cfgA, lock); err != nil {
		t.Fatal(err)
	}
	got, err := BundleForConfig(art, cfgA, "linux/amd64")
	if err != nil || got.BundleDigest != a.BundleDigest {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := BundleForConfig(art, cfgB, "linux/amd64"); err == nil {
		t.Fatal("uncompiled config resolved to a bundle")
	}
	if _, err := BundleForConfig(art, cfgA, "linux/arm64"); err == nil {
		t.Fatal("missing platform resolved")
	}
}
