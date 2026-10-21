package builder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
)

// spool is a compressed NAR downloading into a temporary file, as several
// Range requests at once when it is large, while it is read from the
// front: the reader gets every byte as soon as the bytes before it are on
// disk. Downloaded data waits in the file (the page cache, in practice),
// not in memory, so neither the number of downloads in flight nor a slow
// decoder costs heap.
type spool struct {
	file   *os.File
	url    string
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed when every request has stopped

	mu   sync.Mutex
	cond *sync.Cond
	// size is -1 until known. The file is cut into chunks of chunk bytes
	// each written front to back by one request, written[i] bytes so far;
	// a file not fetched in ranges is a single chunk.
	size    int64
	chunk   int64
	written []int64
	err     error
}

const unknownSize = -1

// rangeSupport records, per substituter, whether it answered a Range
// request with 206: unknown until then, so the first large file probes it.
type rangeSupport int32

const (
	rangeUnknown rangeSupport = iota
	rangeYes
	rangeNo
)

func (f *fetcher) rangesFor(sub string) rangeSupport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ranges[sub]
}

func (f *fetcher) setRanges(sub string, r rangeSupport) {
	f.mu.Lock()
	f.ranges[sub] = r
	f.mu.Unlock()
}

// openSpool starts downloading sub/rel into a temporary file in dir.
// size is the compressed size from the narinfo, 0 when it does not say.
func (f *fetcher) openSpool(dir, sub, rel string, size int64) (*spool, error) {
	file, err := os.CreateTemp(dir, ".nar-*.tmp")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(f.ctx)
	s := &spool{file: file, url: sub + "/" + rel, ctx: ctx, cancel: cancel, done: make(chan struct{}), size: unknownSize}
	s.cond = sync.NewCond(&s.mu)
	ranged := size >= rangeMin && f.rangesFor(sub) != rangeNo
	switch {
	case ranged:
		s.size, s.chunk = size, rangeChunk
	case size > 0:
		s.size, s.chunk = size, size
	default:
		s.chunk = 1 << 62
	}
	s.written = make([]int64, (max(s.size, 1)+s.chunk-1)/s.chunk)
	go func() {
		defer close(s.done)
		s.finish(f.download(s, sub))
	}()
	return s, nil
}

// download runs the requests of s; with more than one chunk, rangeWindow
// of them at a time, in order.
func (f *fetcher) download(s *spool, sub string) error {
	n := len(s.written)
	first := 0
	if f.rangesFor(sub) != rangeYes {
		// one request first: a server that ignores Range answers it with
		// the whole file, which is then simply read to the end
		whole, err := f.fetchChunk(s, 0, true)
		if err != nil || whole {
			return err
		}
		f.setRanges(sub, rangeYes)
		if n == 1 {
			return nil
		}
		first = 1
	}
	var next atomic.Int64
	next.Store(int64(first))
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	for range min(rangeWindow, n-first) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				if _, err := f.fetchChunk(s, i, false); err != nil {
					if errors.Is(err, errNoRange) {
						f.setRanges(sub, rangeNo)
					}
					select {
					case errs <- err:
					default:
					}
					s.cancel()
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

var copyBufs = sync.Pool{New: func() any { return new([64 << 10]byte) }}

// fetchChunk writes chunk i, resuming after what an earlier attempt wrote.
// With mayBeWhole, a 200 answer is the whole file and turns s into a single
// chunk; it reports that.
func (f *fetcher) fetchChunk(s *spool, i int, mayBeWhole bool) (whole bool, err error) {
	s.mu.Lock()
	start, size := int64(i)*s.chunk, s.size
	s.mu.Unlock()
	end := int64(-1) // last byte, inclusive; -1 when the size is unknown
	if size != unknownSize {
		end = min(start+s.chunk, size) - 1
	}
	buf := copyBufs.Get().(*[64 << 10]byte)
	defer copyBufs.Put(buf)
	err = retry(s.ctx, func() error {
		s.mu.Lock()
		off := start + s.written[i]
		s.mu.Unlock()
		if end >= 0 && off > end {
			return nil
		}
		var rng string
		switch {
		case end >= 0:
			rng = fmt.Sprintf("bytes=%d-%d", off, end)
		case off > 0:
			rng = fmt.Sprintf("bytes=%d-", off)
		}
		resp, err := f.do(s.ctx, s.url, rng)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusPartialContent:
			if err := checkContentRange(resp.Header.Get("Content-Range"), off, end, size); err != nil {
				return fmt.Errorf("GET %s: %w", s.url, err)
			}
		case http.StatusOK:
			if off != 0 || !mayBeWhole {
				return errNoRange
			}
			whole = true
			s.mu.Lock()
			s.chunk, s.written = 1<<62, s.written[:1]
			s.mu.Unlock()
			if size != unknownSize {
				end = size - 1
			}
		default:
			return statusErr(s.url, resp)
		}
		for {
			n, rerr := resp.Body.Read(buf[:])
			if n > 0 {
				if _, err := s.file.WriteAt(buf[:n], off); err != nil {
					return err
				}
				off += int64(n)
				s.mu.Lock()
				s.written[i] += int64(n)
				s.cond.Broadcast()
				s.mu.Unlock()
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return rerr
			}
		}
		if end >= 0 && off <= end {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	return whole, err
}

// checkContentRange accepts "bytes off-end/size"; end and size may be
// unknown (-1).
func checkContentRange(cr string, off, end, size int64) error {
	var gotOff, gotEnd int64
	var total string
	if _, err := fmt.Sscanf(cr, "bytes %d-%d/%s", &gotOff, &gotEnd, &total); err != nil || gotOff != off ||
		(end >= 0 && gotEnd != end) || (size != unknownSize && total != fmt.Sprint(size)) {
		return fmt.Errorf("Content-Range %q does not match bytes %d-%d of %d", cr, off, end, size)
	}
	return nil
}

// finish records the outcome of the download and wakes the reader.
func (s *spool) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && s.size == unknownSize {
		s.size = s.written[0]
	}
	if err == nil && s.size != unknownSize {
		// a single chunk that got a 200 knows its size only now
		var n int64
		for _, w := range s.written {
			n += w
		}
		if n != s.size {
			err = fmt.Errorf("GET %s: got %d bytes, want %d", s.url, n, s.size)
		}
	}
	s.err = err
	s.cond.Broadcast()
}

// Reader returns the file from the start; reads wait for the download.
func (s *spool) Reader() io.Reader { return &spoolReader{s: s} }

type spoolReader struct {
	s   *spool
	pos int64
}

func (r *spoolReader) Read(p []byte) (int, error) {
	s := r.s
	s.mu.Lock()
	var avail int64
	for {
		if s.size != unknownSize && r.pos >= s.size {
			s.mu.Unlock()
			return 0, io.EOF
		}
		c := r.pos / s.chunk
		avail = c*s.chunk + s.written[c] - r.pos
		if avail > 0 {
			break
		}
		if s.err != nil {
			s.mu.Unlock()
			return 0, s.err
		}
		select {
		case <-s.done:
			// finished without error yet short: finish reports it
			s.mu.Unlock()
			return 0, io.ErrUnexpectedEOF
		default:
		}
		s.cond.Wait()
	}
	s.mu.Unlock()
	n, err := s.file.ReadAt(p[:min(int64(len(p)), avail)], r.pos)
	r.pos += int64(n)
	if err == io.EOF {
		err = nil
	}
	return n, err
}

// Close stops the download and removes the file.
func (s *spool) Close() error {
	s.cancel()
	<-s.done
	s.file.Close()
	return os.Remove(s.file.Name())
}
