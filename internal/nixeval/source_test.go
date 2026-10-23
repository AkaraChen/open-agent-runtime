package nixeval

import (
	"archive/tar"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// smallNixpkgs is a source archive with a handful of files.
func smallNixpkgs(t *testing.T) []byte {
	t.Helper()
	top := "nixpkgs-" + rev
	return archive(t, top, []tar.Header{
		{Name: top + "/default.nix", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: top + "/pkgs/top-level/all.nix", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{
		top + "/default.nix":            "import ./pkgs/top-level/all.nix",
		top + "/pkgs/top-level/all.nix": "{ }",
	})
}

func useCache(t *testing.T) string {
	t.Helper()
	cacheRoot = t.TempDir()
	t.Cleanup(func() { cacheRoot = "" })
	return cacheRoot
}

// leftovers lists the temporary directories a download leaves in the cache.
func leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(CacheDir(), "nixpkgs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			left = append(left, e.Name())
		}
	}
	return left
}

// The build asks for the tree from every platform's nix stage and then from
// the nix-eval child: callers of one process that arrive together download
// it once.
func TestSourceConcurrentCallersShareOneDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	data := smallNixpkgs(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(150 * time.Millisecond) // long enough for all callers to arrive
		w.Write(data)
	}))
	defer srv.Close()
	useCache(t)

	const callers = 8
	var wg sync.WaitGroup
	dirs := make([]string, callers)
	errs := make([]error, callers)
	logs := make([]strings.Builder, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dirs[i], errs[i] = Source(context.Background(), rev, srv.URL, &logs[i])
		}()
	}
	close(start)
	wg.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if dirs[i] != SourceDir(rev) {
			t.Fatalf("caller %d: dir %s", i, dirs[i])
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("downloaded %d times", n)
	}
	b, err := os.ReadFile(filepath.Join(SourceDir(rev), "pkgs", "top-level", "all.nix"))
	if err != nil || string(b) != "{ }" {
		t.Fatalf("all.nix: %q %v", b, err)
	}
	// exactly one of them did the work and said so
	said := 0
	for i := range logs {
		if strings.Contains(logs[i].String(), "downloading nixpkgs@") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("%d callers logged a download", said)
	}
	if left := leftovers(t); len(left) != 0 {
		t.Fatalf("temporary directories left: %v", left)
	}
}

// A caller that gives up stops waiting at once, and the download it waited
// for is not affected.
func TestSourceWaiterGivesUp(t *testing.T) {
	data := smallNixpkgs(t)
	var hits atomic.Int32
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		arrived <- struct{}{}
		<-release
		w.Write(data)
	}))
	defer srv.Close()
	useCache(t)

	first := make(chan error, 1)
	go func() {
		_, err := Source(context.Background(), rev, srv.URL, nil)
		first <- err
	}()
	<-arrived // the first download is under way
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := Source(ctx, rev, srv.URL, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a waiter that ran out of time got %v", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Fatal("the waiter was not released by its context")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := Source(context.Background(), rev, srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("downloaded %d times", n)
	}
}

// A build that was stopped (a sibling stage failed) does not start a
// download, even when nobody else holds the directory.
func TestSourceStoppedCallerDoesNotDownload(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	useCache(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 20; i++ { // the lock is free every time
		if _, err := Source(ctx, rev, srv.URL, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests", n)
	}
	if _, err := os.Stat(filepath.Join(CacheDir(), "nixpkgs")); err == nil {
		t.Fatal("a stopped caller got as far as making the download directory")
	}
}

// A download that fails leaves nothing behind and does not stop the next
// caller from trying again.
func TestSourceRetriesAfterFailure(t *testing.T) {
	data := smallNixpkgs(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch hits.Add(1) {
		case 1:
			http.Error(w, "try later", http.StatusServiceUnavailable)
		case 2:
			w.Write(data[:len(data)/2]) // cut short
		default:
			w.Write(data)
		}
	}))
	defer srv.Close()
	useCache(t)

	for i, want := range []string{"503", "extract nixpkgs"} {
		if _, err := Source(context.Background(), rev, srv.URL, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("attempt %d: got %v, want an error with %q", i+1, err, want)
		}
		if _, err := os.Stat(SourceDir(rev)); err == nil {
			t.Fatalf("attempt %d left a checkout behind", i+1)
		}
		if left := leftovers(t); len(left) != 0 {
			t.Fatalf("attempt %d left temporary directories: %v", i+1, left)
		}
	}
	if _, err := Source(context.Background(), rev, srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("%d requests", n)
	}
}

// Another process may publish the same revision while this one downloads it:
// whoever renames first wins, and the other finds a complete tree.
func TestSourceLosesTheRaceGracefully(t *testing.T) {
	data := smallNixpkgs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// the other process finishes while this response is on its way
		other := filepath.Join(SourceDir(rev), "default.nix")
		if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(other, []byte("from the other process"), 0o644); err != nil {
			t.Error(err)
		}
		w.Write(data)
	}))
	defer srv.Close()
	useCache(t)

	dir, err := Source(context.Background(), rev, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "default.nix"))
	if err != nil || string(b) != "from the other process" {
		t.Fatalf("the published tree was replaced: %q %v", b, err)
	}
	if left := leftovers(t); len(left) != 0 {
		t.Fatalf("temporary directories left: %v", left)
	}
}
