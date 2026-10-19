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
