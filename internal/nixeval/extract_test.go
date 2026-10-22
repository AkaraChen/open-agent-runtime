package nixeval

import (
	"archive/tar"
	"bytes"
	stdgzip "compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// extractSerial is how the archive was extracted before the writers ran in
// parallel: one entry after the other, from the goroutine that reads it. It
// is the reference the parallel extractor has to match.
func extractSerial(r io.Reader, dst string) (int, error) {
	gz, err := stdgzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		if h.Name == "pax_global_header" {
			continue
		}
		if path.IsAbs(h.Name) || slices.Contains(strings.Split(h.Name, "/"), "..") {
			return files, fmt.Errorf("archive entry %q escapes the destination", h.Name)
		}
		_, rel, ok := strings.Cut(path.Clean(h.Name), "/")
		if !ok || rel == "" || rel == "." {
			continue // the top-level directory itself
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return files, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return files, err
			}
			files++
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return files, err
			}
			files++
		default:
			// hard links and devices do not occur in a source archive
		}
	}
}

// treeNode is what the comparison looks at of a file system object.
type treeNode struct {
	Kind string // dir, file, link, other
	Perm fs.FileMode
	Data string
	Link string
}

// snapshot lists the tree under root with everything the extractor decides:
// what each path is, its permission bits, the content of files and the target
// of links. Modification times are left out on purpose: they were never kept.
func snapshot(t *testing.T, root string) map[string]treeNode {
	t.Helper()
	out := map[string]treeNode{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		n := treeNode{Perm: info.Mode().Perm()}
		switch {
		case info.IsDir():
			n.Kind = "dir"
		case info.Mode()&fs.ModeSymlink != 0:
			n.Kind = "link"
			n.Perm = 0 // links have no mode of their own
			if n.Link, err = os.Readlink(p); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			n.Kind = "file"
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			n.Data = string(b)
		default:
			n.Kind = "other"
		}
		out[filepath.ToSlash(rel)] = n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffTrees(t *testing.T, want, got map[string]treeNode) {
	t.Helper()
	var paths []string
	for p := range want {
		paths = append(paths, p)
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	bad := 0
	for _, p := range paths {
		w, wok := want[p]
		g, gok := got[p]
		switch {
		case !gok:
			t.Errorf("%s: missing (want a %s)", p, w.Kind)
		case !wok:
			t.Errorf("%s: unexpected %s", p, g.Kind)
		case w.Kind != g.Kind || w.Perm != g.Perm || w.Link != g.Link:
			t.Errorf("%s: got %s %v -> %q, want %s %v -> %q", p, g.Kind, g.Perm, g.Link, w.Kind, w.Perm, w.Link)
		case w.Data != g.Data:
			t.Errorf("%s: content differs (%d bytes, want %d)", p, len(g.Data), len(w.Data))
		default:
			continue
		}
		if bad++; bad >= 20 {
			t.Fatal("too many differences")
		}
	}
}

// tarEntry is one entry of a test archive. Names are relative to the
// top-level directory unless raw is set.
type tarEntry struct {
	hdr  tar.Header
	data []byte
	raw  bool
}

func writeArchive(t *testing.T, top string, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := stdgzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": "0123456789abcdef"}}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		h := e.hdr
		if !e.raw {
			h.Name = top + "/" + h.Name
		}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.data))
		}
		if h.Mode == 0 && h.Typeflag == tar.TypeReg {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fileEntry(name string, mode int64, data []byte) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode}, data: data}
}

func dirEntry(name string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}}
}

func linkEntry(name, target string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}}
}

// noise returns n bytes that do not compress.
func noise(seed byte, n int) []byte {
	var key [32]byte
	key[0] = seed
	b := make([]byte, n)
	rand.NewChaCha8(key).Read(b)
	return b
}

var (
	syntheticOnce sync.Once
	syntheticData []byte
)

// syntheticArchive is built once for all the tests that extract it.
func syntheticArchive(t *testing.T) []byte {
	t.Helper()
	syntheticOnce.Do(func() { syntheticData = buildSyntheticArchive(t) })
	if syntheticData == nil {
		t.Fatal("the synthetic archive could not be built")
	}
	return syntheticData
}

// buildSyntheticArchive covers what a source archive has and some it does
// not: nested and empty directories, files whose directory entry is missing
// or comes late, sizes from nothing to a quarter MB, every mode the extractor
// maps, links to files, directories and nothing, repeated paths, entries it
// skips and enough small files for the writers to share.
func buildSyntheticArchive(t *testing.T) []byte {
	var es []tarEntry
	es = append(es,
		dirEntry("a"), dirEntry("a/b"), dirEntry("a/b/c"),
		fileEntry("a/b/c/deep.nix", 0o644, []byte("{ }\n")),
		fileEntry("a/top.txt", 0o644, []byte("top")),
		dirEntry("empty"), dirEntry("empty/deeper"),
		fileEntry("implicit/x/y/z.txt", 0o644, []byte("no directory entries before this")),
		fileEntry("late/dir/f.txt", 0o644, []byte("f")),
		dirEntry("late/dir"), // after the file in it
		dirEntry("dir with space"),
		fileEntry("dir with space/файл.txt", 0o644, []byte("unicode")),
		fileEntry(strings.Repeat("long-directory-name/", 12)+"leaf.txt", 0o644, []byte("PAX extended header")),
	)
	for i, size := range []int{0, 1, 511, 512, 513, 4095, 4096, 4097, 65535, 65536, 65537, 256<<10 - 1, 256<<10 + 7} {
		es = append(es, fileEntry(fmt.Sprintf("sizes/f%02d_%d", i, size), 0o644, noise(byte(i), size)))
	}
	es = append(es, fileEntry("sizes/zeros", 0o644, make([]byte, 512<<10)))
	for i, mode := range []int64{0o644, 0o755, 0o600, 0o700, 0o444, 0o111, 0o4755, 0o010, 0o664} {
		es = append(es, fileEntry(fmt.Sprintf("modes/m%d", i), mode, []byte{byte(i)}))
	}
	es = append(es,
		linkEntry("links/to-file", "../a/top.txt"),
		linkEntry("links/to-dir", "../a/b"),
		linkEntry("links/dangling", "nowhere/at/all"),
		linkEntry("links/absolute", "/nonexistent/absolute/target"),
		linkEntry("rootlink", "a"),
		dirEntry("links/sub"),
		linkEntry("links/sub/up", "../../a/b/c/deep.nix"),
		// the same path twice: the last entry wins
		fileEntry("again/f", 0o644, []byte("first")),
		fileEntry("again/f", 0o755, []byte("second, longer")),
		fileEntry("again/g", 0o755, []byte("kept executable")),
		fileEntry("again/g", 0o644, []byte("g")),
		linkEntry("again/l", "one"),
		linkEntry("again/l", "two"),
		fileEntry("again/swap", 0o644, []byte("file first")),
		linkEntry("again/swap", "then-link"),
		dirEntry("again/d"),
		dirEntry("again/d"),
		// entries the extractor skips
		tarEntry{hdr: tar.Header{Name: "skipped/hard", Typeflag: tar.TypeLink, Linkname: "a/top.txt"}},
		tarEntry{hdr: tar.Header{Name: "skipped/dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
		tarEntry{hdr: tar.Header{Name: "skipped/fifo", Typeflag: tar.TypeFifo}},
	)
	// many small files across many directories, in the order git lists them
	for d := 0; d < 100; d++ {
		dn := fmt.Sprintf("many/d%03d", d)
		es = append(es, dirEntry(dn))
		for f := 0; f < 16; f++ {
			size := (d*31 + f*17) % 3000
			mode := int64(0o644)
			if f%7 == 0 {
				mode = 0o755
			}
			es = append(es, fileEntry(fmt.Sprintf("%s/file%02d.nix", dn, f), mode, noise(byte(d+f), size)))
		}
		if d%25 == 0 {
			es = append(es, linkEntry(dn+"/link", "file00.nix"))
		}
	}
	return writeArchive(t, "nixpkgs-0123456789abcdef", es)
}

func TestExtractMatchesSerial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks, modes")
	}
	data := syntheticArchive(t)
	want := t.TempDir()
	wantFiles, err := extractSerial(bytes.NewReader(data), want)
	if err != nil {
		t.Fatal(err)
	}
	wantTree := snapshot(t, want)
	if len(wantTree) < 1500 {
		t.Fatalf("the reference tree has %d entries; the archive is too small to share between writers", len(wantTree))
	}
	for _, c := range []struct {
		name string
		opt  extractOptions
	}{
		{"default", extractOptions{}},
		{"one writer", extractOptions{workers: 1}},
		{"entry by entry", extractOptions{workers: 8, batchJobs: 1}},
		{"tiny batches", extractOptions{workers: 3, batchJobs: 2, batchBytes: 1}},
		// the buffer holds a few small files; bigger ones stream from the
		// reading goroutine
		{"small buffer", extractOptions{workers: 4, bufferMax: 96 << 10}},
		{"small buffer, one writer", extractOptions{workers: 1, bufferMax: 8 << 10, batchJobs: 1}},
		{"many writers", extractOptions{workers: 32}},
		// tar blocks and file contents cross the boundaries of the pieces
		// the inflated stream is passed on in
		{"small chunks", extractOptions{chunk: 513}},
		{"tiny chunks", extractOptions{workers: 2, chunk: 7, batchJobs: 4}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := t.TempDir()
			n, err := extractTarGzWith(bytes.NewReader(data), got, c.opt)
			if err != nil {
				t.Fatal(err)
			}
			if n != wantFiles {
				t.Errorf("wrote %d files and links, the serial extractor %d", n, wantFiles)
			}
			diffTrees(t, wantTree, snapshot(t, got))
		})
	}
}

// The defaults of Source are one of the cases above, but they depend on the
// machine: also run what a build runs, repeatedly, for the race detector and
// for orderings the others do not reach.
func TestExtractRepeatedly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks, modes")
	}
	data := syntheticArchive(t)
	want := t.TempDir()
	if _, err := extractSerial(bytes.NewReader(data), want); err != nil {
		t.Fatal(err)
	}
	wantTree := snapshot(t, want)
	for i := 0; i < 3; i++ {
		got := t.TempDir()
		if _, err := extractTarGz(bytes.NewReader(data), got); err != nil {
			t.Fatal(err)
		}
		diffTrees(t, wantTree, snapshot(t, got))
	}
}

func TestExtractRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	const top = "nixpkgs-0123456789abcdef"
	for _, c := range []struct {
		name    string
		entries func(outside string) []tarEntry
		want    string
	}{
		{"dot dot", func(string) []tarEntry {
			return []tarEntry{fileEntry("default.nix", 0o644, nil), fileEntry("../evil", 0o644, []byte("x"))}
		}, "escapes"},
		{"dot dot in the middle", func(string) []tarEntry {
			return []tarEntry{fileEntry("a/../../evil", 0o644, []byte("x"))}
		}, "escapes"},
		{"absolute", func(string) []tarEntry {
			return []tarEntry{{hdr: tar.Header{Name: "/tmp/evil", Typeflag: tar.TypeReg, Mode: 0o644}, data: []byte("x"), raw: true}}
		}, "escapes"},
		{"file below a link", func(outside string) []tarEntry {
			return []tarEntry{linkEntry("l", outside), fileEntry("l/pwned", 0o644, []byte("x"))}
		}, "through a symlink"},
		{"file below a link, other case", func(outside string) []tarEntry {
			return []tarEntry{linkEntry("Dir", outside), fileEntry("dir/pwned", 0o644, []byte("x"))}
		}, "through a symlink"},
		{"directory below a link", func(outside string) []tarEntry {
			return []tarEntry{linkEntry("l", outside), dirEntry("l/sub")}
		}, "through a symlink"},
		{"link below a link", func(outside string) []tarEntry {
			return []tarEntry{linkEntry("l", outside), linkEntry("l/x", "y")}
		}, "through a symlink"},
		{"file where a link is", func(outside string) []tarEntry {
			return []tarEntry{linkEntry("l", filepath.Join(outside, "target")), fileEntry("l", 0o644, []byte("x"))}
		}, "through a symlink"},
	} {
		t.Run(c.name, func(t *testing.T) {
			outside := t.TempDir()
			data := writeArchive(t, top, c.entries(outside))
			dst := t.TempDir()
			_, err := extractTarGz(bytes.NewReader(data), dst)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
			if left, _ := os.ReadDir(outside); len(left) != 0 {
				t.Fatalf("something was written outside the destination: %v", left)
			}
		})
	}
}

// Links are extracted as they are, also those to directories.
func TestExtractKeepsLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	data := writeArchive(t, "top", []tarEntry{
		dirEntry("pkgs"), fileEntry("pkgs/real.nix", 0o644, []byte("real")),
		linkEntry("alias", "pkgs"), linkEntry("pkgs/self", "real.nix"),
		dirEntry("other"), fileEntry("other/x", 0o644, []byte("x")),
	})
	dst := t.TempDir()
	if _, err := extractTarGz(bytes.NewReader(data), dst); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "alias")); err != nil || l != "pkgs" {
		t.Fatalf("alias: %q %v", l, err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "alias", "real.nix")); err != nil || string(b) != "real" {
		t.Fatalf("through the link: %q %v", b, err)
	}
}

func TestExtractErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks, modes")
	}
	data := syntheticArchive(t)
	for _, c := range []struct {
		name string
		data []byte
		opt  extractOptions
	}{
		{"truncated", data[:len(data)/2], extractOptions{}},
		{"truncated, one writer, entry by entry", data[:len(data)/3], extractOptions{workers: 1, batchJobs: 1}},
		{"cut after the header", data[:12], extractOptions{}},
		{"not gzip", []byte("this is not an archive"), extractOptions{}},
		{"empty", nil, extractOptions{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dst := t.TempDir()
			done := make(chan error, 1)
			go func() { _, err := extractTarGzWith(bytes.NewReader(c.data), dst, c.opt); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a damaged archive was accepted")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("extraction hangs")
			}
			// nothing writes into the directory after the call has returned
			before := snapshot(t, dst)
			time.Sleep(20 * time.Millisecond)
			diffTrees(t, before, snapshot(t, dst))
		})
	}
}

// A failing writer stops the extraction, however much is queued behind it
// and however little buffer is left for the reader.
func TestExtractWriterFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes")
	}
	es := []tarEntry{
		fileEntry("a", 0o644, []byte("a file")),
		fileEntry("a/b", 0o644, []byte("below a file")), // the directory "a" cannot be made
	}
	for i := 0; i < 3000; i++ {
		es = append(es, fileEntry(fmt.Sprintf("many/f%04d", i), 0o644, noise(byte(i), 4096)))
	}
	data := writeArchive(t, "top", es)
	for _, opt := range []extractOptions{
		{},
		{workers: 1, batchJobs: 1, bufferMax: 16 << 10},
		{workers: 2, batchJobs: 3, bufferMax: 8 << 10},
	} {
		dst := t.TempDir()
		done := make(chan error, 1)
		go func() { _, err := extractTarGzWith(bytes.NewReader(data), dst, opt); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%+v: a file below a file was accepted", opt)
			}
			var pe *fs.PathError
			if !errors.As(err, &pe) {
				t.Fatalf("%+v: got %T %v, want the file system's error", opt, err, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%+v: extraction hangs", opt)
		}
	}
}

// The writers need nothing from each other, so the order in which they get
// to the entries must not matter: entries of one directory are run by
// different writers, and a directory entry may come after its files.
func TestExtractDirectoryOrder(t *testing.T) {
	var es []tarEntry
	for i := 0; i < 400; i++ {
		d := fmt.Sprintf("deep/%d/%d/%d", i%7, i%13, i)
		es = append(es, fileEntry(d+"/f", 0o644, []byte{byte(i)}), dirEntry(d), dirEntry("deep/"+fmt.Sprint(i%7)))
	}
	data := writeArchive(t, "top", es)
	want := t.TempDir()
	if _, err := extractSerial(bytes.NewReader(data), want); err != nil {
		t.Fatal(err)
	}
	for _, opt := range []extractOptions{{workers: 8, batchJobs: 1}, {workers: 4, batchJobs: 5}, {}} {
		got := t.TempDir()
		if _, err := extractTarGzWith(bytes.NewReader(data), got, opt); err != nil {
			t.Fatal(err)
		}
		diffTrees(t, snapshot(t, want), snapshot(t, got))
	}
}
