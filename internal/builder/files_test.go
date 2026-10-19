package builder

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

var aLongTimeAgo = time.Unix(1000, 0)

func newTestBuilder(t *testing.T, sources map[string]*spec.Source) (*Builder, string) {
	dir := t.TempDir()
	cfg := &spec.Config{Dir: dir, Sources: sources}
	return &Builder{ctx: context.Background(), cfg: cfg, opts: Options{Artifacts: filepath.Join(dir, "artifacts"), Log: io.Discard}}, dir
}

func TestImportDirAndFileDeterministic(t *testing.T) {
	b, dir := newTestBuilder(t, map[string]*spec.Source{
		"conf":  {Path: "conf.json", Target: "/etc/my-agent/config.json"},
		"tools": {Path: "tools", Target: "/opt/tools", PathEntries: []string{"/opt/tools/bin"}},
	})
	os.WriteFile(filepath.Join(dir, "conf.json"), []byte(`{"a":1}`), 0o600)
	os.MkdirAll(filepath.Join(dir, "tools", "bin"), 0o755)
	os.WriteFile(filepath.Join(dir, "tools", "bin", "hello"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	os.Symlink("hello", filepath.Join(dir, "tools", "bin", "hi"))

	f, err := b.buildComponent(spec.Ref{Kind: spec.KindFile, Name: "conf"}, arm64)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"etc/", "etc/my-agent/", "etc/my-agent/config.json"}
	if len(f.Files) != len(want) {
		t.Fatalf("files %v", f.Files)
	}
	d1, err := b.buildComponent(spec.Ref{Kind: spec.KindDir, Name: "tools"}, arm64)
	if err != nil {
		t.Fatal(err)
	}
	// touching mtimes must not change the content key
	os.Chtimes(filepath.Join(dir, "tools", "bin", "hello"), aLongTimeAgo, aLongTimeAgo)
	d2, err := b.buildComponent(spec.Ref{Kind: spec.KindDir, Name: "tools"}, arm64)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Key != d2.Key {
		t.Fatal("component key is not deterministic")
	}
	found := map[string]bool{}
	for _, p := range d1.Files {
		found[p] = true
	}
	for _, p := range []string{"opt/", "opt/tools/", "opt/tools/bin/", "opt/tools/bin/hello", "opt/tools/bin/hi"} {
		if !found[p] {
			t.Fatalf("missing %s in %v", p, d1.Files)
		}
	}
	if _, err := b.buildComponent(spec.Ref{Kind: spec.KindBinary, Name: "tools"}, arm64); err == nil {
		t.Fatal("binary: pointing at a directory should fail")
	}
}

func TestImportBinaryELF(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	b, dir := newTestBuilder(t, map[string]*spec.Source{
		"hello": {Path: "hello-{{arch}}", Build: []string{"go", "-C", "src", "build", "-o", "../hello-{{arch}}", "."},
			BuildEnv: map[string]string{"GOOS": "linux", "GOARCH": "{{arch}}", "CGO_ENABLED": "0", "GOFLAGS": "-mod=mod"}},
	})
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "go.mod"), []byte("module hello\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
	c, err := b.buildComponent(spec.Ref{Kind: spec.KindBinary, Name: "hello"}, arm64)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ELF) != 1 || c.ELF[0].Machine != "arm64" || !c.ELF[0].Static || c.Target != "/opt/hello" {
		t.Fatalf("elf %+v target %s", c.ELF, c.Target)
	}
}

func TestImportInlineContent(t *testing.T) {
	cfg := "{\"listen\": \":8080\"}\n"
	script := "#!/bin/sh\necho hi\n"
	b, _ := newTestBuilder(t, map[string]*spec.Source{
		"conf":  {Content: &cfg, Target: "/etc/my-agent/config.json"},
		"hello": {Content: &script, Target: "/usr/local/bin/hello"},
	})
	c, err := b.buildComponent(spec.Ref{Kind: spec.KindFile, Name: "conf"}, arm64)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Files[len(c.Files)-1]; got != "etc/my-agent/config.json" {
		t.Fatalf("files %v", c.Files)
	}
	tarPath := filepath.Join(b.opts.Artifacts, filepath.FromSlash(c.Path))
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	modes := map[string]int64{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		modes[h.Name] = h.Mode
		if h.Name == "etc/my-agent/config.json" {
			body, _ := io.ReadAll(tr)
			if string(body) != cfg {
				t.Fatalf("content %q", body)
			}
		}
	}
	if modes["etc/my-agent/config.json"] != 0o644 {
		t.Fatalf("file mode %o", modes["etc/my-agent/config.json"])
	}
	again, _ := b.buildComponent(spec.Ref{Kind: spec.KindFile, Name: "conf"}, arm64)
	if again.Key != c.Key {
		t.Fatal("inline content key not deterministic")
	}
	h, err := b.buildComponent(spec.Ref{Kind: spec.KindBinary, Name: "hello"}, arm64)
	if err != nil {
		t.Fatal(err)
	}
	if h.Target != "/usr/local/bin/hello" || len(h.ELF) != 0 {
		t.Fatalf("%+v", h)
	}
}
