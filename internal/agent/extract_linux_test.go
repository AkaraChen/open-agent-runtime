//go:build linux

package agent

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func mkTar(t *testing.T, hdrs []*tar.Header, bodies map[string]string) *bytes.Buffer {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		h.ModTime = time.Unix(1, 0)
		if b, ok := bodies[h.Name]; ok {
			h.Size = int64(len(b))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if b, ok := bodies[h.Name]; ok {
			tw.Write([]byte(b))
		}
	}
	tw.Close()
	return &buf
}

func TestExtract(t *testing.T) {
	dest := t.TempDir()
	buf := mkTar(t, []*tar.Header{
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o555},
		{Name: "usr/bin/tool", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "usr/bin"},
		{Name: "usr/bin/tool2", Typeflag: tar.TypeLink, Linkname: "usr/bin/tool"},
	}, map[string]string{"usr/bin/tool": "#!/bin/sh\n"})
	if _, err := extractTar(buf, dest); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dest, "usr/bin"))
	if err != nil || st.Mode().Perm() != 0o555 {
		t.Fatalf("dir mode: %v %v", st.Mode(), err)
	}
	if l, _ := os.Readlink(filepath.Join(dest, "bin")); l != "usr/bin" {
		t.Fatal("symlink")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "usr/bin/tool2")); string(b) != "#!/bin/sh\n" {
		t.Fatal("hardlink")
	}
	os.Chmod(filepath.Join(dest, "usr/bin"), 0o755)
}

func TestExtractRefusesSymlinkTraversal(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	buf := mkTar(t, []*tar.Header{
		{Name: "etc", Typeflag: tar.TypeSymlink, Linkname: outside},
		{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"etc/passwd": "pwned"})
	if _, err := extractTar(buf, dest); err == nil {
		t.Fatal("expected traversal error")
	}
	if _, err := os.Stat(filepath.Join(outside, "passwd")); err == nil {
		t.Fatal("wrote outside destination")
	}
}

func TestExtractDotDot(t *testing.T) {
	dest := t.TempDir()
	buf := mkTar(t, []*tar.Header{{Name: "../../escape", Typeflag: tar.TypeReg, Mode: 0o644}}, map[string]string{"../../escape": "x"})
	if _, err := extractTar(buf, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "escape")); err != nil {
		t.Fatal("'..' should be clamped inside dest")
	}
}

// BenchmarkExtract extracts every tar in $OAR_BENCH_TARS (for example a
// closure's store artifacts) into a fresh directory per iteration.
func BenchmarkExtract(b *testing.B) {
	dir := os.Getenv("OAR_BENCH_TARS")
	if dir == "" {
		b.Skip("set OAR_BENCH_TARS to a directory of tar files")
	}
	tars, _ := filepath.Glob(filepath.Join(dir, "*.tar"))
	var data [][]byte
	for _, p := range tars {
		d, err := os.ReadFile(p)
		if err != nil {
			b.Fatal(err)
		}
		data = append(data, d)
	}
	root := b.TempDir()
	if r := os.Getenv("OAR_BENCH_ROOT"); r != "" {
		root = r
	}
	files := 0
	i := 0
	for b.Loop() {
		dest := filepath.Join(root, fmt.Sprintf("x%d", i))
		i++
		if err := os.MkdirAll(dest, 0o755); err != nil {
			b.Fatal(err)
		}
		files = 0
		for _, d := range data {
			n, err := extractTar(bytes.NewReader(d), dest)
			if err != nil {
				b.Fatal(err)
			}
			files += n
		}
		// untimed: clean up and write back, so iterations do not run into
		// the kernel's dirty-page throttling
		b.StopTimer()
		os.RemoveAll(dest)
		syncDir(root)
		b.StartTimer()
	}
	b.ReportMetric(float64(files), "files/op")
}

func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		unix.Syncfs(int(f.Fd()))
		f.Close()
	}
}
