package nixeval

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/gzip"
	"golang.org/x/sync/semaphore"
)

// extractOptions tunes extractTarGz; the zero value is what Source uses.
// Tests shrink the limits to reach the rare paths.
type extractOptions struct {
	// workers is the number of goroutines that write files.
	workers int
	// bufferMax bounds the file contents that have been read from the
	// archive and not yet written. A file larger than half of it is written
	// by the reading goroutine as it streams past, so memory stays bounded
	// whatever the archive holds.
	bufferMax int64
	// batchJobs and batchBytes close a batch of entries handed to a writer.
	batchJobs  int
	batchBytes int64
	// chunk is the size of the pieces the inflated stream is passed on in.
	chunk int
}

func (o extractOptions) withDefaults() extractOptions {
	if o.workers <= 0 {
		// PERF: a writer's work is system calls, and they scale with the
		// writers until the goroutines that inflate and parse have no CPU
		// left. mkdir is the dear one on ext4 (25 to 30 us of kernel time,
		// 37,000 of them in nixpkgs; a file is 15 us). Extracting nixpkgs on
		// 4 CPUs took 1.4 s with 2 writers, 1.3 s with 4 to 8, and 3.4 s
		// with the 1 goroutine that did everything before.
		o.workers = min(max(runtime.GOMAXPROCS(0), 2), 8)
	}
	if o.bufferMax <= 0 {
		o.bufferMax = 64 << 20
	}
	if o.batchJobs <= 0 {
		// PERF: handing a writer one entry at a time costs the reading
		// goroutine, the bottleneck, a goroutine wake-up per entry: 90,000
		// of them for nixpkgs. A batch of 32 entries (or 128 KiB) is one
		// wake-up. Extracting nixpkgs took 1.9 s with batches of one entry
		// and 1.5 s with 32.
		o.batchJobs = 32
	}
	if o.batchBytes <= 0 {
		o.batchBytes = 128 << 10
	}
	if o.chunk <= 0 {
		o.chunk = 256 << 10
	}
	return o
}

// extractQueue is how many batches may wait for a writer. It is deep because
// the writers' pace varies, a run of directories being slower than a run of
// small files, and a deep queue lets the reader go on through the slow
// stretches. The memory that matters is the file contents, which bufferMax
// bounds.
const extractQueue = 256

// extractTarGz writes a GitHub-style archive (one top-level directory) into
// dst without that directory. Entries that would land outside dst, or be
// written through a symlink of the archive, are refused. Symlinks are kept as
// they are: nothing here follows them. It returns the number of files and
// symlinks written.
//
// Modes are 0644, or 0755 when the archive marks the file executable, and
// directories are 0755 (all before the umask); modification times are not
// kept. The result is that of extracting the entries one after the other,
// but the work is a pipeline of three kinds of goroutine: one inflates the
// stream, one (the caller's) parses the tar and reads the files' contents,
// and a few writers create the directories and files.
//
// PERF: nixpkgs is 51 MB of gzip and 250 MB of tar, in 37,000 directories and
// 53,000 files. Written file by file from one goroutine, which is what most
// of the 6.4 s of "Nix evaluation" on a machine that has never built is (CI
// kvm runner), extracting it took 3.4 to 3.7 s on a 4 to 8 core dev machine,
// from the page cache. Inflating was the smaller part (1.4 to 1.6 s with
// compress/gzip, 0.9 to 1.1 s with klauspost's), then parsing the tar
// (about 0.4 s); the rest was system calls, and the 32 KiB buffer io.Copy
// allocates for every file (1.7 GB of garbage). Now it takes 1.3 s. The
// inflater and the parser are apart because together they were the longest
// chain: 1.15 s with the writers left out, and 0.8 s as two goroutines.
func extractTarGz(r io.Reader, dst string) (int, error) {
	return extractTarGzWith(r, dst, extractOptions{})
}

func extractTarGzWith(r io.Reader, dst string, opt extractOptions) (int, error) {
	opt = opt.withDefaults()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	in := startInflater(gz, opt.chunk)
	defer in.stop()
	x := newExtractor(dst, opt)
	if err := x.read(tar.NewReader(in)); err != nil {
		x.fail(err)
	}
	return x.finish()
}

// inflater passes on a gzip stream, which a goroutine of its own inflates a
// little ahead of whoever reads it. It owns the gzip reader from the start.
type inflater struct {
	chunks chan []byte // inflated data, in order; closed when the stream ends
	free   chan []byte // chunks that were read and can be filled again
	done   chan struct{}
	err    error // why chunks was closed, set before that

	cur []byte // the chunk being read
	buf []byte // and the whole of its buffer
}

// inflateAhead is how many chunks the inflater may run ahead.
const inflateAhead = 8

func startInflater(gz *gzip.Reader, chunk int) *inflater {
	in := &inflater{
		chunks: make(chan []byte, inflateAhead),
		free:   make(chan []byte, inflateAhead+2),
		done:   make(chan struct{}),
	}
	// one buffer for each chunk that can be queued, one being read and one
	// being filled, so that returning a buffer never blocks
	for range inflateAhead + 2 {
		in.free <- make([]byte, chunk)
	}
	go in.run(gz)
	return in
}

func (in *inflater) run(gz *gzip.Reader) {
	defer close(in.chunks)
	defer gz.Close()
	for {
		var buf []byte
		select {
		case buf = <-in.free:
		case <-in.done:
			return
		}
		n := 0
		var err error
		for n < len(buf) && err == nil {
			var m int
			m, err = gz.Read(buf[n:])
			n += m
		}
		if n > 0 {
			select {
			case in.chunks <- buf[:n]:
			case <-in.done:
				return
			}
		}
		if err != nil {
			in.err = err // io.EOF at the end of the stream
			return
		}
	}
}

func (in *inflater) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(in.cur) == 0 {
		if in.buf != nil {
			in.free <- in.buf[:cap(in.buf)]
			in.buf = nil
		}
		c, ok := <-in.chunks
		if !ok {
			if in.err == nil {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, in.err
		}
		in.buf, in.cur = c, c
	}
	n := copy(p, in.cur)
	in.cur = in.cur[n:]
	return n, nil
}

// stop ends the inflating, which may be a read of the network away from
// noticing: closing what it reads from ends that.
func (in *inflater) stop() { close(in.done) }

// extractJob is one archive entry for a writer.
type extractJob struct {
	kind   byte // tar.TypeDir, tar.TypeReg or tar.TypeSymlink
	target string
	mode   os.FileMode // files
	data   []byte      // files: the whole content
	link   string      // symlinks: where they point
}

// extractBatch is what the reader hands over: entries in archive order, and
// the share of the buffer limit their contents hold.
type extractBatch struct {
	jobs  []extractJob
	bytes int64
}

type extractor struct {
	opt  extractOptions
	root string
	// ctx ends when the first error is recorded, so that nobody waits for
	// work that will not be done.
	ctx    context.Context
	cancel context.CancelFunc
	// buffered counts the bytes of file content between the reader and the
	// writers. The reader takes them before it reads a file, the writer that
	// is done with the batch returns them.
	buffered *semaphore.Weighted
	batches  chan *extractBatch
	writers  sync.WaitGroup
	// queued counts the batches that were handed over and are not done.
	queued sync.WaitGroup
	files  atomic.Int64

	errMu sync.Mutex
	err   error

	// What follows belongs to the reading goroutine.
	cur *extractBatch
	// written holds the paths of the files and symlinks queued so far, and
	// links those of the symlinks, both lower-cased: a case-insensitive
	// filesystem would take "A/x" for a file of the link "a".
	written map[string]struct{}
	links   map[string]struct{}
}

func newExtractor(dst string, opt extractOptions) *extractor {
	ctx, cancel := context.WithCancel(context.Background())
	x := &extractor{
		opt:      opt,
		root:     dst,
		ctx:      ctx,
		cancel:   cancel,
		buffered: semaphore.NewWeighted(opt.bufferMax),
		batches:  make(chan *extractBatch, extractQueue),
		written:  map[string]struct{}{},
		links:    map[string]struct{}{},
	}
	x.cur = x.newBatch()
	x.writers.Add(opt.workers)
	for range opt.workers {
		go x.write()
	}
	return x
}

func (x *extractor) newBatch() *extractBatch {
	return &extractBatch{jobs: make([]extractJob, 0, x.opt.batchJobs)}
}

// fail records the first error and stops the rest of the work.
func (x *extractor) fail(err error) {
	x.errMu.Lock()
	defer x.errMu.Unlock()
	if x.err == nil {
		x.err = err
		x.cancel()
	}
}

func (x *extractor) firstErr() error {
	x.errMu.Lock()
	defer x.errMu.Unlock()
	return x.err
}

// finish hands over the last entries, waits until every writer is done (the
// caller removes the directory next, and nothing may be writing into it),
// and returns the file count and the first error.
func (x *extractor) finish() (int, error) {
	x.flush()
	close(x.batches)
	x.writers.Wait()
	x.cancel()
	return int(x.files.Load()), x.firstErr()
}

// read parses the archive and queues its entries.
func (x *extractor) read(tr *tar.Reader) error {
	for {
		if x.ctx.Err() != nil {
			return x.firstErr() // a writer failed
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Name == "pax_global_header" {
			continue
		}
		if escapes(h.Name) {
			return fmt.Errorf("archive entry %q escapes the destination", h.Name)
		}
		_, rel, ok := strings.Cut(path.Clean(h.Name), "/")
		if !ok || rel == "" || rel == "." {
			continue // the top-level directory itself
		}
		var kind byte
		switch h.Typeflag {
		case tar.TypeDir, tar.TypeReg, tar.TypeSymlink:
			kind = h.Typeflag
		default:
			continue // hard links and devices do not occur in a source archive
		}
		key := strings.ToLower(rel)
		if len(x.links) > 0 && x.throughLink(key, kind == tar.TypeReg) {
			return fmt.Errorf("archive entry %q would be written through a symlink", h.Name)
		}
		target := filepath.Join(x.root, filepath.FromSlash(rel))
		switch kind {
		case tar.TypeDir:
			x.add(extractJob{kind: kind, target: target})
		case tar.TypeSymlink:
			x.ordered(key)
			x.links[key] = struct{}{}
			x.add(extractJob{kind: kind, target: target, link: h.Linkname})
		case tar.TypeReg:
			x.ordered(key)
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			if err := x.queueFile(tr, h.Size, target, mode); err != nil {
				return err
			}
		}
	}
}

// escapes reports whether an entry name is absolute or climbs out with "..".
func escapes(name string) bool {
	if path.IsAbs(name) {
		return true
	}
	for rest := name; ; {
		seg, more, found := strings.Cut(rest, "/")
		if seg == ".." {
			return true
		}
		if !found {
			return false
		}
		rest = more
	}
}

// throughLink reports whether a file or directory at key would be reached
// through a symlink the archive made: one of its ancestors is a link, or, for
// a file, the path itself is (creating a file follows a link in the last
// place too).
func (x *extractor) throughLink(key string, file bool) bool {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			if _, ok := x.links[key[:i]]; ok {
				return true
			}
		}
	}
	if file {
		_, ok := x.links[key]
		return ok
	}
	return false
}

// ordered keeps "the last entry for a path wins" when an archive names a path
// twice. Writers run side by side, so a second entry waits until everything
// before it is done. Archives from git hold every path once, and then this is
// a map insert.
func (x *extractor) ordered(key string) {
	if _, dup := x.written[key]; dup {
		x.flush()
		x.queued.Wait()
	}
	x.written[key] = struct{}{}
}

// queueFile reads a file's content and queues it. A file too big for the
// buffer is written right here instead.
func (x *extractor) queueFile(tr io.Reader, size int64, target string, mode os.FileMode) error {
	if size > x.opt.bufferMax/2 {
		return x.streamFile(tr, target, mode)
	}
	var data []byte
	if size > 0 {
		if !x.buffered.TryAcquire(size) {
			// the writers can only free what they were given
			x.flush()
			if err := x.buffered.Acquire(x.ctx, size); err != nil {
				return x.firstErr()
			}
		}
		x.cur.bytes += size
		data = make([]byte, size)
		if _, err := io.ReadFull(tr, data); err != nil {
			return err
		}
	}
	x.add(extractJob{kind: tar.TypeReg, target: target, mode: mode, data: data})
	return nil
}

func (x *extractor) streamFile(r io.Reader, target string, mode os.FileMode) error {
	create := func() (*os.File, error) {
		return os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	}
	f, err := create()
	if errors.Is(err, fs.ErrNotExist) {
		if err = mkdirAll(filepath.Dir(target)); err == nil {
			f, err = create()
		}
	}
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		x.files.Add(1)
	}
	return err
}

// add queues an entry, and hands the batch over when it is full.
func (x *extractor) add(j extractJob) {
	x.cur.jobs = append(x.cur.jobs, j)
	if len(x.cur.jobs) >= x.opt.batchJobs || x.cur.bytes >= x.opt.batchBytes {
		x.flush()
	}
}

// flush hands the current batch to the writers. It blocks while the queue is
// full, which is what keeps the reader from running away.
func (x *extractor) flush() {
	if len(x.cur.jobs) == 0 && x.cur.bytes == 0 {
		return
	}
	b := x.cur
	x.cur = x.newBatch()
	x.queued.Add(1)
	x.batches <- b
}

// write is a writer goroutine. After a failure it still takes batches and
// gives back their share of the buffer, so that nothing blocks.
func (x *extractor) write() {
	defer x.writers.Done()
	for b := range x.batches {
		if x.ctx.Err() == nil {
			for i := range b.jobs {
				if err := x.run(&b.jobs[i]); err != nil {
					x.fail(err)
					break
				}
			}
		}
		if b.bytes > 0 {
			x.buffered.Release(b.bytes)
		}
		x.queued.Done()
	}
}

// run performs one entry. The directory a file goes into is made when the
// file finds it missing: entries of one directory are in different writers'
// hands, so a directory entry may not have been run yet. Every step is safe
// to repeat or to race with the same step in another writer.
func (x *extractor) run(j *extractJob) error {
	switch j.kind {
	case tar.TypeDir:
		return mkdirAll(j.target)
	case tar.TypeReg:
		err := os.WriteFile(j.target, j.data, j.mode)
		if errors.Is(err, fs.ErrNotExist) {
			if err = mkdirAll(filepath.Dir(j.target)); err == nil {
				err = os.WriteFile(j.target, j.data, j.mode)
			}
		}
		if err != nil {
			return err
		}
	case tar.TypeSymlink:
		_ = os.Remove(j.target)
		err := os.Symlink(j.link, j.target)
		if errors.Is(err, fs.ErrNotExist) {
			if err = mkdirAll(filepath.Dir(j.target)); err == nil {
				err = os.Symlink(j.link, j.target)
			}
		}
		if err != nil {
			return err
		}
	}
	x.files.Add(1)
	return nil
}

// mkdirAll is os.MkdirAll for the case this program is in nearly every time:
// the parent exists, so one system call makes the directory.
func mkdirAll(dir string) error {
	err := os.Mkdir(dir, 0o755)
	if err == nil {
		return nil
	}
	if parent := filepath.Dir(dir); errors.Is(err, fs.ErrNotExist) && parent != dir {
		if perr := mkdirAll(parent); perr != nil {
			return perr
		}
		if err = os.Mkdir(dir, 0o755); err == nil {
			return nil
		}
	}
	// made by another writer meanwhile, or a file is in the way
	if st, serr := os.Lstat(dir); serr == nil && st.IsDir() {
		return nil
	}
	return err
}
