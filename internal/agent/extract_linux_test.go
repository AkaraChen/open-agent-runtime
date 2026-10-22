//go:build linux

package agent

import (
	"archive/tar"
	"bytes"
	"fmt"
	"math/rand"
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

// synthTar returns a tar of nfiles regular files shaped like a store path:
// directories two levels deep, mostly tiny read-only files (a fifth of them
// executable), one symlink per eight files, and a few files larger than the
// copy buffer. It is the same bytes every time, owned by the process's own
// ids (as root-owned files are for the root agent), so runs are comparable.
// The directories stay writable so that a run as an ordinary user can
// remove what it extracted.
func synthTar(tb testing.TB, nfiles int) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	put := func(h *tar.Header, data []byte) {
		h.ModTime, h.Uid, h.Gid, h.Size = time.Unix(1, 0), euid, egid, int64(len(data))
		if err := tw.WriteHeader(h); err != nil {
			tb.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			tb.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(1))
	sizes := [...]int{0, 8, 24, 60, 100, 100, 180, 400, 900, 3000}
	payload := bytes.Repeat([]byte("0123456789abcdef"), 300<<10/16)
	n := 0
	for top := 0; n < nfiles; top++ {
		put(&tar.Header{Name: fmt.Sprintf("d%03d/", top), Typeflag: tar.TypeDir, Mode: 0o755}, nil)
		for sub := 0; sub < 5 && n < nfiles; sub++ {
			put(&tar.Header{Name: fmt.Sprintf("d%03d/s%d/", top, sub), Typeflag: tar.TypeDir, Mode: 0o755}, nil)
			for i := 0; i < 20 && n < nfiles; i++ {
				name := fmt.Sprintf("d%03d/s%d/f%02d", top, sub, i)
				size, mode := sizes[rng.Intn(len(sizes))], int64(0o444)
				if n%1000 == 999 {
					size = len(payload)
				}
				if n%5 == 0 {
					mode = 0o555
				}
				put(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode}, payload[:size])
				if n%8 == 0 {
					put(&tar.Header{Name: name + ".lnk", Typeflag: tar.TypeSymlink, Linkname: filepath.Base(name), Mode: 0o777}, nil)
				}
				n++
			}
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// BenchmarkExtract extracts every tar in $OAR_BENCH_TARS (for example a
// closure's store artifacts) into a fresh directory per iteration and
// writes it back. Without it, a generated tar of 5000 small files is used.
func BenchmarkExtract(b *testing.B) {
	var data [][]byte
	if dir := os.Getenv("OAR_BENCH_TARS"); dir != "" {
		tars, _ := filepath.Glob(filepath.Join(dir, "*.tar"))
		for _, p := range tars {
			d, err := os.ReadFile(p)
			if err != nil {
				b.Fatal(err)
			}
			data = append(data, d)
		}
	} else {
		data = append(data, synthTar(b, 5000))
	}
	b.ReportAllocs()
	root := b.TempDir()
	if r := os.Getenv("OAR_BENCH_ROOT"); r != "" {
		root = r
	}
	files, i := 0, 0
	var cpu time.Duration
	for b.Loop() {
		dest := filepath.Join(root, fmt.Sprintf("x%d", i))
		i++
		if err := os.MkdirAll(dest, 0o755); err != nil {
			b.Fatal(err)
		}
		c0 := cpuTime()
		files = 0
		for _, d := range data {
			n, err := extractTar(bytes.NewReader(d), dest)
			if err != nil {
				b.Fatal(err)
			}
			files += n
		}
		// the time to write it back, then untimed cleanup
		syncDir(root)
		cpu += cpuTime() - c0
		b.StopTimer()
		os.RemoveAll(dest)
		syncDir(root)
		b.StartTimer()
	}
	b.ReportMetric(float64(files), "files/op")
	// CPU time is steadier than the wall clock on a busy machine
	b.ReportMetric(float64(cpu.Microseconds())/1000/float64(i), "cpu-ms/op")
}

func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		unix.Syncfs(int(f.Fd()))
		f.Close()
	}
}

func TestExtractModesAndTimes(t *testing.T) {
	dest := t.TempDir()
	mt := time.Unix(1_000_000, 0)
	hs := []*tar.Header{
		{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o555},
		{Name: "d/sub/", Typeflag: tar.TypeDir, Mode: 0o700},
		{Name: "d/sub/f", Typeflag: tar.TypeReg, Mode: 0o4755},
		{Name: "d/link", Typeflag: tar.TypeSymlink, Linkname: "sub/f"},
		{Name: "d/fifo", Typeflag: tar.TypeFifo, Mode: 0o664},
		{Name: "sticky/", Typeflag: tar.TypeDir, Mode: 0o1777},
		{Name: "implicit/x/f", Typeflag: tar.TypeReg, Mode: 0o644},
	}
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, h := range hs {
		h.ModTime = time.Unix(1, 0)
		if h.Typeflag == tar.TypeDir || h.Typeflag == tar.TypeSymlink {
			h.ModTime = mt
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = 1
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	if _, err := extractTar(&out, dest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dest, "d"), 0o755) })
	for p, want := range map[string]os.FileMode{
		"d":        0o555,
		"d/sub":    0o700,
		"d/sub/f":  0o755 | os.ModeSetuid,
		"d/fifo":   0o664 | os.ModeNamedPipe,
		"sticky":   0o777 | os.ModeSticky,
		"implicit": 0o755 &^ umask,
	} {
		st, err := os.Lstat(filepath.Join(dest, p))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode() &^ os.ModeDir; got != want {
			t.Errorf("%s: mode %v, want %v", p, got, want)
		}
	}
	for _, p := range []string{"d", "d/sub", "d/link", "sticky"} {
		st, err := os.Lstat(filepath.Join(dest, p))
		if err != nil {
			t.Fatal(err)
		}
		if !st.ModTime().Equal(mt) {
			t.Errorf("%s: mtime %v, want %v (directories are stamped after their children)", p, st.ModTime(), mt)
		}
	}
	if l, _ := os.Readlink(filepath.Join(dest, "d/link")); l != "sub/f" {
		t.Errorf("symlink target %q", l)
	}
}

func TestExtractOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dest := t.TempDir()
	buf := mkTar(t, []*tar.Header{
		{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1234, Gid: 4321},
		{Name: "d/f", Typeflag: tar.TypeReg, Mode: 0o644, Uid: 1234, Gid: 4321},
		{Name: "d/l", Typeflag: tar.TypeSymlink, Linkname: "f", Uid: 1234, Gid: 4321},
		{Name: "r/", Typeflag: tar.TypeDir, Mode: 0o555},
	}, nil)
	if _, err := extractTar(buf, dest); err != nil {
		t.Fatal(err)
	}
	for p, ids := range map[string][2]uint32{"d": {1234, 4321}, "d/f": {1234, 4321}, "d/l": {1234, 4321}, "r": {0, 0}} {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(dest, p), &st); err != nil {
			t.Fatal(err)
		}
		if st.Uid != ids[0] || st.Gid != ids[1] {
			t.Errorf("%s: owner %d:%d, want %d:%d", p, st.Uid, st.Gid, ids[0], ids[1])
		}
	}
}

func TestExtractDuplicates(t *testing.T) {
	dest := t.TempDir()
	buf := mkTar(t, []*tar.Header{
		{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "a", Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "b/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "b/x", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "a"}, // replaces the directory
		{Name: "c/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "c/", Typeflag: tar.TypeDir, Mode: 0o750},
	}, map[string]string{"b/x": "x"})
	if _, err := extractTar(buf, dest); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dest, "a")); st.Mode().Perm() != 0o600 {
		t.Errorf("a mode %v", st.Mode())
	}
	if l, err := os.Readlink(filepath.Join(dest, "b")); err != nil || l != "a" {
		t.Errorf("b: %q %v", l, err)
	}
	if st, _ := os.Stat(filepath.Join(dest, "c")); st.Mode().Perm() != 0o750 {
		t.Errorf("c mode %v", st.Mode())
	}
}

// A directory replaced by a symlink must not stay writable through the
// cached descriptors.
func TestExtractReplacedDirIsNotTraversed(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	buf := mkTar(t, []*tar.Header{
		{Name: "a/b/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "a/b/f", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "a", Typeflag: tar.TypeSymlink, Linkname: outside},
		{Name: "a/b/g", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"a/b/f": "1", "a/b/g": "2"})
	if _, err := extractTar(buf, dest); err == nil {
		t.Fatal("expected traversal error")
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Fatalf("wrote outside destination: %v", es)
	}
}

func TestExtractHardlinks(t *testing.T) {
	for name, hs := range map[string][]*tar.Header{
		"to directory":    {{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755}, {Name: "l", Typeflag: tar.TypeLink, Linkname: "d"}},
		"missing target":  {{Name: "l", Typeflag: tar.TypeLink, Linkname: "nope"}},
		"through symlink": {{Name: "s", Typeflag: tar.TypeSymlink, Linkname: "/"}, {Name: "l", Typeflag: tar.TypeLink, Linkname: "s/etc/passwd"}},
	} {
		if _, err := extractTar(mkTar(t, hs, nil), t.TempDir()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// '..' is clamped inside dest, like entry names
	dest := t.TempDir()
	buf := mkTar(t, []*tar.Header{
		{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "l1", Typeflag: tar.TypeLink, Linkname: "../../f"},
		{Name: "l2", Typeflag: tar.TypeLink, Linkname: "l1"}, // link to a link
	}, map[string]string{"f": "data"})
	if _, err := extractTar(buf, dest); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Stat(filepath.Join(dest, "f"))
	for _, p := range []string{"l1", "l2"} {
		if l, _ := os.Stat(filepath.Join(dest, p)); !os.SameFile(f, l) {
			t.Errorf("%s is not a link to f", p)
		}
	}
	// a target that existed before the extraction
	if err := os.WriteFile(filepath.Join(dest, "pre"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractTar(mkTar(t, []*tar.Header{{Name: "l3", Typeflag: tar.TypeLink, Linkname: "pre"}}, nil), dest); err != nil {
		t.Fatal(err)
	}
}

// More directories than descriptors are kept open.
func TestExtractManyDirs(t *testing.T) {
	dest := t.TempDir()
	var hs []*tar.Header
	for i := 0; i < 3*maxDirFds; i++ {
		hs = append(hs, &tar.Header{Name: fmt.Sprintf("d%d/", i), Typeflag: tar.TypeDir, Mode: 0o755})
		hs = append(hs, &tar.Header{Name: fmt.Sprintf("d%d/e/f", i), Typeflag: tar.TypeReg, Mode: 0o644})
	}
	if _, err := extractTar(mkTar(t, hs, nil), dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, fmt.Sprintf("d%d/e/f", 3*maxDirFds-1))); err != nil {
		t.Fatal(err)
	}
}
