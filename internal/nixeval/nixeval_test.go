package nixeval

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExpressionAttrs(t *testing.T) {
	e := Expression("/tmp/nix pkgs", Request{System: "aarch64-linux", Attrs: []string{"hello", "python313"}})
	for _, want := range []string{
		`import (/. + "/tmp/nix pkgs")`,
		`system = "aarch64-linux"`,
		`builtins.fromJSON "[\"hello\",\"python313\"]"`,
		`mainProgram = d.meta.mainProgram or ""`,
	} {
		if !strings.Contains(e, want) {
			t.Errorf("expression lacks %q:\n%s", want, e)
		}
	}
}

func TestExpressionExpr(t *testing.T) {
	e := Expression("/n", Request{System: "x86_64-linux", Expr: " pkgs.hello.version "})
	if !strings.HasSuffix(e, "in (pkgs.hello.version)\n") {
		t.Errorf("expression form:\n%s", e)
	}
	if strings.Contains(e, "fromJSON") {
		t.Errorf("expression form carries the attribute walk:\n%s", e)
	}
}

func TestParseAttrs(t *testing.T) {
	good := []byte(`{"hello":{"outputs":["/nix/store/abc-hello"],"mainProgram":"hello"}}`)
	res, err := ParseAttrs(good, []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res["hello"].MainProgram != "hello" || res["hello"].Outputs[0] != "/nix/store/abc-hello" {
		t.Fatalf("parsed %+v", res)
	}
	if _, err := ParseAttrs(good, []string{"hello", "missing"}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing attribute not reported: %v", err)
	}
	bad := []byte(`{"x":{"outputs":["/home/me/x"]}}`)
	if _, err := ParseAttrs(bad, []string{"x"}); err == nil || !strings.Contains(err.Error(), "not a store path") {
		t.Fatalf("non-store path not reported: %v", err)
	}
	if _, err := ParseAttrs([]byte("nope"), nil); err == nil {
		t.Fatal("garbage accepted")
	}
}

// archive builds a GitHub-style tar.gz with one top-level directory.
func archive(t *testing.T, top string, entries []tar.Header, contents map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader}
	_ = tw.WriteHeader(&hdr)
	_ = tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, h := range entries {
		h := h
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(contents[h.Name]))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(contents[h.Name])); err != nil {
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

const rev = "0123456789abcdef0123456789abcdef01234567"

func TestSourceDownloadsOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	top := "nixpkgs-" + rev
	data := archive(t, top, []tar.Header{
		{Name: top + "/default.nix", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: top + "/pkgs/top-level/all.nix", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: top + "/pkgs/run.sh", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: top + "/link.nix", Typeflag: tar.TypeSymlink, Linkname: "default.nix"},
	}, map[string]string{
		top + "/default.nix":            "import ./pkgs/top-level/all.nix",
		top + "/pkgs/top-level/all.nix": "{ }",
		top + "/pkgs/run.sh":            "#!/bin/sh\n",
	})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/"+rev+".tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	t.Setenv("OAR_NIXPKGS_MIRROR", srv.URL)
	t.Setenv("OAR_CACHE_DIR", t.TempDir())
	var log bytes.Buffer
	dir, err := Source(context.Background(), rev, &log)
	if err != nil {
		t.Fatal(err)
	}
	if dir != SourceDir(rev) {
		t.Fatalf("dir %s", dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "default.nix"))
	if err != nil || string(b) != "import ./pkgs/top-level/all.nix" {
		t.Fatalf("default.nix: %q %v", b, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "pkgs", "run.sh")); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("exec bit lost: %v %v", st, err)
	}
	if l, err := os.Readlink(filepath.Join(dir, "link.nix")); err != nil || l != "default.nix" {
		t.Fatalf("symlink: %q %v", l, err)
	}
	if !strings.Contains(log.String(), "downloading nixpkgs@") {
		t.Fatalf("log: %q", log.String())
	}
	if _, err := Source(context.Background(), rev, nil); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("downloaded %d times", n)
	}
	// a local checkout is used as it is
	local, err := Source(context.Background(), dir, nil)
	if err != nil || local != dir {
		t.Fatalf("local checkout: %s %v", local, err)
	}
	if _, err := Source(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("a directory without default.nix was accepted")
	}
}

func TestSourceRefusesEscape(t *testing.T) {
	top := "nixpkgs-" + rev
	data := archive(t, top, []tar.Header{
		{Name: top + "/default.nix", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: top + "/../evil", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{top + "/default.nix": "{}", top + "/../evil": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer srv.Close()
	t.Setenv("OAR_NIXPKGS_MIRROR", srv.URL)
	cache := t.TempDir()
	t.Setenv("OAR_CACHE_DIR", cache)
	if _, err := Source(context.Background(), rev, nil); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escape not refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "nixpkgs", "evil")); err == nil {
		t.Fatal("escaping entry was written")
	}
	if _, err := os.Stat(SourceDir(rev)); err == nil {
		t.Fatal("a failed extraction left the checkout in place")
	}
}

// fakeOar writes a shell script that stands in for `oar nix-eval`.
func fakeOar(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	p := filepath.Join(t.TempDir(), "oar")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestChildSuccess(t *testing.T) {
	exe := fakeOar(t, `echo "args: $*" >&2; echo '{"hello":{"outputs":["/nix/store/x-hello"]}}'`)
	var log bytes.Buffer
	c := Child{Executable: exe, Backend: "embedded", Log: &log}
	out, err := c.Eval(context.Background(), Request{Nixpkgs: rev, System: "x86_64-linux", Attrs: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]Result
	if err := json.Unmarshal(out, &v); err != nil || v["hello"].Outputs[0] != "/nix/store/x-hello" {
		t.Fatalf("%s %v", out, err)
	}
	want := "args: nix-eval --json --system x86_64-linux --nixpkgs " + rev + " --backend embedded -- hello"
	if !strings.Contains(log.String(), want) {
		t.Fatalf("child args: %q", log.String())
	}
}

func TestChildEvalError(t *testing.T) {
	exe := fakeOar(t, `echo "error: attribute 'nope' missing" >&2; exit 1`)
	c := Child{Executable: exe}
	_, err := c.Eval(context.Background(), Request{Nixpkgs: rev, System: "x86_64-linux", Attrs: []string{"nope"}})
	if err == nil || !strings.Contains(err.Error(), "attribute 'nope' missing") {
		t.Fatalf("stderr not in the error: %v", err)
	}
	if strings.Contains(err.Error(), "crashed") {
		t.Fatalf("an exit status is not a crash: %v", err)
	}
}

func TestChildCrash(t *testing.T) {
	exe := fakeOar(t, `echo "thread 'iets-eval' panicked at src/eval/mod.rs" >&2; kill -ABRT $$`)
	c := Child{Executable: exe}
	_, err := c.Eval(context.Background(), Request{Nixpkgs: rev, System: "x86_64-linux", Attrs: []string{"hello"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"crashed", "panicked", "OAR_NIX=local"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestChildCancel(t *testing.T) {
	exe := fakeOar(t, `sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := Child{Executable: exe}
	_, err := c.Eval(ctx, Request{Nixpkgs: rev, System: "x86_64-linux", Attrs: []string{"hello"}})
	if err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}
