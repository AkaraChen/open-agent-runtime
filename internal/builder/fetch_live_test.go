package builder

import (
	"context"
	"flag"
	"io"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

var (
	liveFetch = flag.Bool("live-fetch", false, "download a real closure from cache.nixos.org")
	liveSubs  = flag.String("live-subs", "", "substituter to fetch from instead")
)

// TestLiveFetch measures fetchClosure against the public cache:
// go test ./internal/builder -run LiveFetch -args -live-fetch
func TestLiveFetch(t *testing.T) {
	if !*liveFetch {
		t.Skip("needs -live-fetch")
	}
	roots := []string{
		"/nix/store/b1wn5clix1mz4vpfih7x9cnj9qfh7c6h-git-2.54.0",
		"/nix/store/r8avny0rl4kpc9zig9ar8i9ny7mw0lxz-nodejs-22.23.3",
		"/nix/store/jy00n3jp5xj8s40n1fbldmkcr5n7avxc-python3-3.13.15",
		"/nix/store/4xjwmsbmf2m6pw3hcqipwbxfz1z16pvv-git-2.54.0",
		"/nix/store/sqlzha196wsz4ihj6cmmjhm27dp0hks7-nodejs-22.23.3",
		"/nix/store/q1khrhbn506imjxx7kiqcbihycawa6kl-python3-3.13.15",
	}
	stats := &Stats{}
	cfg := &spec.Config{}
	if *liveSubs != "" {
		cfg.Runtime.Substituters = []string{*liveSubs}
	}
	b := &Builder{ctx: context.Background(), cfg: cfg, opts: Options{Artifacts: t.TempDir(), Log: io.Discard, Stats: stats}}
	start := time.Now()
	got, err := b.fetchClosure(roots)
	if err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	t.Logf("%d paths, %s in %s (%.1f MB/s)", len(got), humanSize(stats.FetchedBytes), d.Round(time.Millisecond),
		float64(stats.FetchedBytes)/1e6/d.Seconds())

	// everything is on disk now: the closure is walked from the metadata
	fetched := stats.FetchedPaths
	again, err := b.fetchClosure(roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(got) || stats.FetchedPaths != fetched {
		t.Errorf("second walk: %d paths, %d downloads; want %d paths, none downloaded",
			len(again), stats.FetchedPaths-fetched, len(got))
	}
}
