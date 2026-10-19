package spec

import (
	"math/rand/v2"
	"reflect"
	"testing"
)

func cfgWith(components []string) *Config {
	return &Config{
		Runtime:    Runtime{Base: "debian:trixie-slim"},
		Components: components,
		Sources: map[string]*Source{
			"my-agent": {Path: "x", Requires: []string{"nix:git"}},
			"conf":     {Path: "y"},
		},
		Process: Process{Command: []string{"/opt/my-agent"}},
	}
}

func TestResolveComponentsOrderInvariant(t *testing.T) {
	base := []string{"nix:nodejs_22", "nix:python313", "nix:git", "binary:my-agent", "file:conf"}
	want, err := cfgWith(base).ResolveComponents()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		perm := append([]string(nil), base...)
		rand.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		got, err := cfgWith(perm).ResolveComponents()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("order %v: got %v want %v", perm, got, want)
		}
	}
}

func TestRequiresExpandAndDedupe(t *testing.T) {
	got, err := cfgWith([]string{"binary:my-agent", "binary:my-agent"}).ResolveComponents()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "binary:my-agent" || got[1].String() != "nix:git" {
		t.Fatalf("got %v", got)
	}
}

func TestCycleAndErrors(t *testing.T) {
	c := cfgWith([]string{"binary:a"})
	c.Sources["a"] = &Source{Path: "a", Requires: []string{"binary:b"}}
	c.Sources["b"] = &Source{Path: "b", Requires: []string{"binary:a"}}
	if _, err := c.ResolveComponents(); err == nil {
		t.Fatal("expected cycle error")
	}
	for _, bad := range []string{"nodejs", "apt:curl", "binary:missing"} {
		if _, err := cfgWith([]string{bad}).ResolveComponents(); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestPlatform(t *testing.T) {
	p, err := ParsePlatform("linux/arm64")
	if err != nil || p.NixSystem() != "aarch64-linux" || p.Slug() != "linux-arm64" {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := ParsePlatform("darwin/arm64"); err == nil {
		t.Fatal("expected error")
	}
	if Expand("bin-{{arch}}-{{os}}", p) != "bin-arm64-linux" {
		t.Fatal("expand")
	}
}

const otherRev = "b6018f87da91d19d0ab4cf979885689b469cdd41"

func TestNixRevisionPin(t *testing.T) {
	r, err := ParseRef("nix:nodejs_22@" + otherRev)
	if err != nil || r.Name != "nodejs_22" || r.Rev != otherRev || r.String() != "nix:nodejs_22@"+otherRev {
		t.Fatalf("%+v %v", r, err)
	}
	c := cfgWith([]string{"nix:git", "nix:nodejs_22@" + otherRev})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.NixRevision(r) != otherRev || c.NixRevision(Ref{Kind: KindNix, Name: "git"}) != DefaultNixpkgsRevision {
		t.Fatal("NixRevision")
	}
	for _, bad := range []string{"nix:git@nixos-unstable", "nix:git@abc123", "nix:@" + otherRev, "binary:my-agent@" + otherRev} {
		if _, err := ParseRef(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
	c = cfgWith([]string{"nix:git"})
	c.Runtime.Nixpkgs = "nixos-26.05"
	if err := c.Validate(); err == nil {
		t.Fatal("branch name accepted as runtime.nixpkgs")
	}
}

func TestInlineContentValidation(t *testing.T) {
	content := "hello\n"
	c := cfgWith([]string{"file:conf"})
	c.Sources["conf"] = &Source{Content: &content, Target: "/etc/x"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Sources["conf"] = &Source{Content: &content, Path: "x"}
	if err := c.Validate(); err == nil {
		t.Fatal("path+content accepted")
	}
	c = cfgWith([]string{"dir:conf"})
	c.Sources["conf"] = &Source{Content: &content}
	if err := c.Validate(); err == nil {
		t.Fatal("dir with content accepted")
	}
	c = cfgWith([]string{"file:conf"})
	c.Sources["conf"] = &Source{}
	if err := c.Validate(); err == nil {
		t.Fatal("source without path or content accepted")
	}
}
