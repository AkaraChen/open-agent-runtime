package builder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// shrinkRanges makes ranged downloads kick in for small test files.
func shrinkRanges(t *testing.T) {
	oldMin, oldChunk, oldWindow := rangeMin, rangeChunk, rangeWindow
	rangeMin, rangeChunk, rangeWindow = 1000, 1000, 3
	t.Cleanup(func() { rangeMin, rangeChunk, rangeWindow = oldMin, oldChunk, oldWindow })
}

func testFetcher(t *testing.T) *fetcher {
	return &fetcher{ctx: context.Background(), client: newDownloadClient(), ranges: map[string]rangeSupport{}}
}

func randomFile(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

type testCache struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string // Range headers, "" for none
}

// newTestCache serves data at /f; handle may answer a request itself.
func newTestCache(t *testing.T, data []byte, handle func(w http.ResponseWriter, r *http.Request) bool) *testCache {
	c := &testCache{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.requests = append(c.requests, r.Header.Get("Range"))
		c.mu.Unlock()
		if handle != nil && handle(w, r) {
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *testCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func readSpool(t *testing.T, f *fetcher, c *testCache, size int64) ([]byte, error) {
	t.Helper()
	s, err := f.openSpool(t.TempDir(), c.URL, "f", size)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(s.Reader())
	if cerr := s.Close(); cerr != nil {
		t.Errorf("close: %v", cerr)
	}
	return got, err
}

func TestSpoolRanged(t *testing.T) {
	shrinkRanges(t)
	data := randomFile(10_500)
	c := newTestCache(t, data, nil)
	f := testFetcher(t)
	got, err := readSpool(t, f, c, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reassembled file differs")
	}
	if n := c.count(); n != 11 {
		t.Errorf("%d requests, want 11", n)
	}
	if f.ranges[c.URL] != rangeYes {
		t.Error("Range support not recorded")
	}
	// known to do ranges now: every chunk starts at once, still 11
	c.requests = nil
	if got, err = readSpool(t, f, c, int64(len(data))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("second read: %v", err)
	}
	if n := c.count(); n != 11 {
		t.Errorf("%d requests on the second file, want 11", n)
	}
}

func TestSpoolNoRangeSupport(t *testing.T) {
	shrinkRanges(t)
	data := randomFile(5_000)
	c := newTestCache(t, data, func(w http.ResponseWriter, r *http.Request) bool {
		w.Write(data)
		return true
	})
	f := testFetcher(t)
	got, err := readSpool(t, f, c, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("whole-file answer read back differently")
	}
	if n := c.count(); n != 1 {
		t.Errorf("%d requests, want 1: the probe's 200 is the file", n)
	}
}

func TestSpoolUnknownSize(t *testing.T) {
	data := randomFile(3_000)
	c := newTestCache(t, data, nil)
	got, err := readSpool(t, testFetcher(t), c, 0)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("err %v, %d bytes", err, len(got))
	}
}

// A connection that breaks mid-chunk resumes where it stopped.
func TestSpoolResumes(t *testing.T) {
	shrinkRanges(t)
	data := randomFile(4_000)
	var broke sync.Once
	c := newTestCache(t, data, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Range") != "bytes=1000-1999" {
			return false
		}
		done := false
		broke.Do(func() {
			w.Header().Set("Content-Range", "bytes 1000-1999/4000")
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[1000:1300])
			done = true
		})
		return done
	})
	got, err := readSpool(t, testFetcher(t), c, int64(len(data)))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("err %v, equal %v", err, bytes.Equal(got, data))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.requests, "bytes=1300-1999") {
		t.Errorf("no resumed request in %q", c.requests)
	}
}

func TestSpoolChunkFailure(t *testing.T) {
	shrinkRanges(t)
	data := randomFile(5_000)
	c := newTestCache(t, data, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Range") == "bytes=3000-3999" {
			http.NotFound(w, r)
			return true
		}
		return false
	})
	got, err := readSpool(t, testFetcher(t), c, int64(len(data)))
	if !errors.Is(err, errNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
	if len(got) > 3000 {
		t.Errorf("read %d bytes, past the failing chunk", len(got))
	}
}

// Closing before the end stops the download and removes the file.
func TestSpoolCloseEarly(t *testing.T) {
	shrinkRanges(t)
	data := randomFile(20_000)
	c := newTestCache(t, data, nil)
	dir := t.TempDir()
	s, err := testFetcher(t).openSpool(dir, c.URL, "f", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(s.Reader(), make([]byte, 1500)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestPrioSemLargestFirst(t *testing.T) {
	s := newPrioSem(1)
	ctx := context.Background()
	if err := s.acquire(ctx, 0); err != nil {
		t.Fatal(err)
	}
	order := make(chan int64, 3)
	var wg sync.WaitGroup
	for i, p := range []int64{5, 50, 10} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.acquire(ctx, p)
			order <- p
			s.release()
		}()
		waitQueued(s, i+1)
	}
	s.release()
	wg.Wait()
	close(order)
	var got []int64
	for p := range order {
		got = append(got, p)
	}
	if want := []int64{50, 10, 5}; !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

func TestPrioSemCancel(t *testing.T) {
	s := newPrioSem(1)
	s.acquire(context.Background(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.acquire(ctx, 1) }()
	waitQueued(s, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	s.release()
	if s.free != 1 || len(s.waiters) != 0 {
		t.Errorf("free %d, waiters %d after cancel", s.free, len(s.waiters))
	}
}

func TestFirstOfKeepsOrder(t *testing.T) {
	op := func(ctx context.Context, sub string) (string, error) {
		switch sub {
		case "miss":
			return "", errNotFound
		case "slow":
			time.Sleep(20 * time.Millisecond)
		}
		return sub, nil
	}
	ctx := context.Background()
	if _, s, _ := firstOf(ctx, []string{"slow", "fast"}, op); s != "slow" {
		t.Errorf("got %q, want the first configured substituter", s)
	}
	if _, s, _ := firstOf(ctx, []string{"miss", "fast"}, op); s != "fast" {
		t.Errorf("got %q, want the fallback", s)
	}
	if _, _, err := firstOf(ctx, []string{"miss", "miss"}, op); err == nil {
		t.Error("no error when every substituter misses")
	}
}

func countQueued(s *prioSem) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waiters)
}

func waitQueued(s *prioSem, n int) {
	for countQueued(s) < n {
		time.Sleep(time.Millisecond)
	}
}
