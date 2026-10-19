package manifest

import (
	"math/rand/v2"
	"testing"
)

func sample() *PlatformLock {
	return &PlatformLock{
		Platform: "linux/arm64",
		Base:     Base{Digest: "sha256:aa", Key: "sha256:bb"},
		Packages: []Package{{Attr: "git", OutPath: "/nix/store/g-git"}, {Attr: "nodejs_22", OutPath: "/nix/store/n-node"}, {Attr: "python313", OutPath: "/nix/store/p-py"}},
		StorePaths: []StorePath{
			{Path: "/nix/store/g-git", NarHash: "h1", Key: "k1"},
			{Path: "/nix/store/n-node", NarHash: "h2", Key: "k2"},
			{Path: "/nix/store/p-py", NarHash: "h3", Key: "k3"},
		},
		Components: []Component{{Name: "my-agent", Kind: "binary", Key: "c1", Target: "/opt/my-agent"}, {Name: "conf", Kind: "file", Key: "c2", Target: "/etc/x"}},
		Process:    Process{Args: []string{"/opt/my-agent"}, Env: []string{"A=1", "PATH=/x", "Z=2"}, Cwd: "/"},
		Hostname:   "oar",
	}
}

func shuffle[T any](s []T) {
	rand.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
}

func TestDigestOrderInvariant(t *testing.T) {
	want := sample().ComputeDigest()
	for i := 0; i < 100; i++ {
		p := sample()
		shuffle(p.Packages)
		shuffle(p.StorePaths)
		shuffle(p.Components)
		shuffle(p.Process.Env)
		if got := p.ComputeDigest(); got != want {
			t.Fatalf("digest changed with order: %s vs %s", got, want)
		}
	}
}

func TestDigestSensitive(t *testing.T) {
	want := sample().ComputeDigest()
	mods := []func(*PlatformLock){
		func(p *PlatformLock) { p.Platform = "linux/amd64" },
		func(p *PlatformLock) { p.Base.Key = "sha256:cc" },
		func(p *PlatformLock) { p.StorePaths[0].NarHash = "other" },
		func(p *PlatformLock) { p.Components[0].Key = "c9" },
		func(p *PlatformLock) { p.Process.Args = []string{"/bin/sh"} },
		func(p *PlatformLock) { p.Process.Env = append(p.Process.Env, "B=3") },
	}
	for i, m := range mods {
		p := sample()
		m(p)
		if p.ComputeDigest() == want {
			t.Fatalf("mod %d did not change the digest", i)
		}
	}
}
