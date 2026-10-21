//go:build linux

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkLinkTree builds hard-link views of the store paths in
// $OAR_BENCH_TARS (extracted once), the way a warm builds a closure's view.
func BenchmarkLinkTree(b *testing.B) {
	dir := os.Getenv("OAR_BENCH_TARS")
	if dir == "" {
		b.Skip("set OAR_BENCH_TARS to a directory of tar files")
	}
	root := b.TempDir()
	if r := os.Getenv("OAR_BENCH_ROOT"); r != "" {
		root = r
	}
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	tars, _ := filepath.Glob(filepath.Join(dir, "*.tar"))
	for _, p := range tars {
		d, err := os.ReadFile(p)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := extractTar(bytes.NewReader(d), src); err != nil {
			b.Fatal(err)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(src, "nix", "store", "*"))
	i := 0
	for b.Loop() {
		dst := filepath.Join(root, "view"+string(rune('a'+i%26)), "x")
		i++
		os.MkdirAll(dst, 0o755)
		for _, p := range paths {
			if err := linkTree(p, filepath.Join(dst, filepath.Base(p))); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		os.RemoveAll(filepath.Dir(dst))
		b.StartTimer()
	}
	b.ReportMetric(float64(len(paths)), "paths/op")
}
