package builder

import (
	"container/heap"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Download tuning. The fetch trades CPU and connections for wall time: a
// closure is a few hundred store paths, most of them small, behind a
// handful of large ones on the critical path. Memory stays flat: NARs are
// spooled to disk (see spool) and only decoding, which is CPU bound, is
// limited to the number of CPUs. Variables so tests can shrink them.
var (
	// narinfo requests are tiny and latency bound; the dependency graph
	// is discovered through them, so they never wait behind NAR bodies.
	// A level of a typical closure is ~120 paths wide.
	narinfoConcurrency = 128
	// store paths whose NAR is downloading at once (largest first).
	narConcurrency = 64
	// NARs of at least rangeMin bytes are fetched as rangeChunk-sized
	// Range requests, up to rangeWindow of them in flight per file, so a
	// large path is not limited to the throughput of one TCP connection.
	rangeMin    int64 = 4 << 20
	rangeChunk  int64 = 2 << 20
	rangeWindow       = 16
	// NARs decoding at once; each holds a zstd/xz window.
	decodeConcurrency = runtime.NumCPU()
	// connections per cache host, shared by everything above.
	maxConnsPerHost = 256
	// attempts per request; the wait doubles from retryBase.
	retryAttempts = 4
	retryBase     = 200 * time.Millisecond
)

// newDownloadClient returns an HTTP/1.1 client with a deep connection pool.
// HTTP/2 would multiplex every request onto one TCP connection: one
// congestion window for the whole closure, and one lost packet stalls every
// stream. Separate connections each get their own (one connection to
// cache.nixos.org gave ~70 MB/s, 16 gave ~250 MB/s).
func newDownloadClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// a non-nil empty map turns HTTP/2 off
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConnsPerHost:   maxConnsPerHost,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}}
}

// errNotFound is a 404: the path is not in that cache, retrying is useless.
var errNotFound = errors.New("not found")

// errNoRange is a 200 answer to a Range request for part of a file.
var errNoRange = errors.New("server ignores Range")

// get returns the 200 response for url, retrying network errors and
// server-side failures with exponential backoff.
func (f *fetcher) get(ctx context.Context, url string) (*http.Response, error) {
	var resp *http.Response
	err := retry(ctx, func() error {
		r, err := f.do(ctx, url, "")
		if err != nil {
			return err
		}
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			return statusErr(url, r)
		}
		resp = r
		return nil
	})
	return resp, err
}

func (f *fetcher) do(ctx context.Context, url, rng string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	return f.client.Do(req)
}

func statusErr(url string, r *http.Response) error {
	err := fmt.Errorf("GET %s: %s", url, r.Status)
	if r.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %w", errNotFound, err)
	}
	return err
}

// retry runs op until it succeeds, fails for good (404, ignored Range,
// cancelled context) or runs out of attempts.
func retry(ctx context.Context, op func() error) error {
	var err error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryBase << (attempt - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = op(); err == nil || errors.Is(err, errNotFound) || errors.Is(err, errNoRange) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// prioSem is a counting semaphore that hands a free slot to the waiter
// with the highest priority.
type prioSem struct {
	mu      sync.Mutex
	free    int
	waiters waitHeap
}

func newPrioSem(n int) *prioSem { return &prioSem{free: n} }

func (s *prioSem) acquire(ctx context.Context, prio int64) error {
	s.mu.Lock()
	if s.free > 0 && len(s.waiters) == 0 {
		s.free--
		s.mu.Unlock()
		return nil
	}
	w := &waiter{prio: prio, ready: make(chan struct{})}
	heap.Push(&s.waiters, w)
	s.mu.Unlock()
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		select {
		case <-w.ready:
			// granted while giving up: pass the slot on
			s.releaseLocked()
		default:
			heap.Remove(&s.waiters, w.index)
		}
		return ctx.Err()
	}
}

func (s *prioSem) release() {
	s.mu.Lock()
	s.releaseLocked()
	s.mu.Unlock()
}

func (s *prioSem) releaseLocked() {
	if len(s.waiters) == 0 {
		s.free++
		return
	}
	close(heap.Pop(&s.waiters).(*waiter).ready)
}

type waiter struct {
	prio  int64
	index int
	ready chan struct{}
}

type waitHeap []*waiter

func (h waitHeap) Len() int           { return len(h) }
func (h waitHeap) Less(i, j int) bool { return h[i].prio > h[j].prio }
func (h waitHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *waitHeap) Push(x any) {
	w := x.(*waiter)
	w.index = len(*h)
	*h = append(*h, w)
}
func (h *waitHeap) Pop() any {
	old := *h
	w := old[len(old)-1]
	*h = old[:len(old)-1]
	return w
}

// firstOf runs op against every substituter at once and returns the result
// of the first one, in configured order, that succeeds: a miss in a
// preferred cache costs no extra round trip before asking the next one.
func firstOf[T any](ctx context.Context, subs []string, op func(ctx context.Context, sub string) (T, error)) (T, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	res := make([]chan result, len(subs))
	for i, s := range subs {
		res[i] = make(chan result, 1)
		go func() {
			v, err := op(ctx, s)
			res[i] <- result{v, err}
		}()
	}
	var zero T
	var errs []string
	for i, s := range subs {
		r := <-res[i]
		if r.err == nil {
			return r.v, s, nil
		}
		errs = append(errs, r.err.Error())
	}
	return zero, "", errors.New(strings.Join(errs, "; "))
}
