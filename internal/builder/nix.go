package builder

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"golang.org/x/sync/errgroup"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/nar"
	"github.com/AkaraChen/open-agent-runtime/internal/nixeval"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

// DefaultSubstituter and DefaultTrustedKey describe the official cache.
const (
	DefaultSubstituter = "https://cache.nixos.org"
	DefaultTrustedKey  = "cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY="
)

// evalResult is the outcome of evaluating one attribute for one system.
type evalResult = nixeval.Result

type evalCache struct {
	Entries map[string]evalResult `json:"entries"`
}

func evalKey(rev, system, attr string) string { return rev + "/" + system + "/" + attr }

// evalPackages resolves attribute names to output store paths. Results are
// cached in artifacts/manifests/nix-eval.json; the evaluator runs only for
// attributes that were never evaluated at this revision, as a child
// `oar nix-eval` process (docs/design/embedded-nix-evaluator.md).
func (b *Builder) evalPackages(p spec.Platform, rev string, attrs []string) (map[string]evalResult, error) {
	cachePath := filepath.Join(b.opts.Artifacts, "manifests", "nix-eval.json")
	var cache evalCache
	_ = manifest.ReadJSON(cachePath, &cache)
	if cache.Entries == nil {
		cache.Entries = map[string]evalResult{}
	}
	system := p.NixSystem()
	out := map[string]evalResult{}
	var missing []string
	for _, a := range attrs {
		if r, ok := cache.Entries[evalKey(rev, system, a)]; ok {
			out[a] = r
		} else {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	b.logf("evaluating nixpkgs@%s for %s: %s", rev[:min(12, len(rev))], system, strings.Join(missing, ", "))
	b.opts.Stats.EvaluatedAttr += len(missing)
	child := nixeval.Child{Log: b.opts.Log}
	res, err := child.Eval(b.ctx, nixeval.Request{Nixpkgs: rev, System: system, Attrs: missing, Mirror: b.cfg.Runtime.NixpkgsMirror})
	if err != nil {
		return nil, err
	}
	parsed, err := nixeval.ParseAttrs(res, missing)
	if err != nil {
		return nil, err
	}
	for _, a := range missing {
		out[a] = parsed[a]
		cache.Entries[evalKey(rev, system, a)] = parsed[a]
	}
	if err := manifest.WriteJSON(cachePath, &cache); err != nil {
		return nil, err
	}
	return out, nil
}

// storeMeta is written next to each store path tar.
type storeMeta struct {
	manifest.StorePath
	Bins []string `json:"bins,omitempty"`
}

type fetcher struct {
	b       *Builder
	ctx     context.Context
	client  *http.Client
	subs    []string
	trusted []string
	// narinfos bounds narinfo requests; nars and decoders hand download
	// and decoding slots to the largest waiting file first, so the big
	// paths that set the critical path start before the many small ones.
	// A NAR gives back its download slot once it is on disk, not when it
	// is decoded: the network never waits for the CPU.
	narinfos chan struct{}
	nars     *prioSem
	decoders *prioSem
	ranges   map[string]rangeSupport
	g        *errgroup.Group
	mu       sync.Mutex
	seen     map[string]bool
	done     map[string]*storeMeta
}

// fetchClosure fetches the runtime closure of roots (full store paths) from
// the binary cache and converts every path into a tar artifact. The graph
// is walked through narinfos as fast as they arrive; each NAR download
// starts as soon as its narinfo is in, without waiting for the rest of its
// level.
func (b *Builder) fetchClosure(roots []string) (map[string]*storeMeta, error) {
	g, ctx := errgroup.WithContext(b.ctx)
	f := &fetcher{
		b:        b,
		ctx:      ctx,
		client:   newDownloadClient(),
		subs:     orDefault(b.cfg.Runtime.Substituters, DefaultSubstituter),
		trusted:  orDefault(b.cfg.Runtime.TrustedKeys, DefaultTrustedKey),
		narinfos: make(chan struct{}, narinfoConcurrency),
		nars:     newPrioSem(narConcurrency),
		decoders: newPrioSem(decodeConcurrency),
		ranges:   map[string]rangeSupport{},
		g:        g,
		seen:     map[string]bool{},
		done:     map[string]*storeMeta{},
	}
	defer f.client.CloseIdleConnections()
	for _, r := range roots {
		f.visit(path.Base(r))
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return f.done, nil
}

// orDefault is v, or def split on whitespace when v is empty.
func orDefault(v []string, def string) []string {
	if len(v) == 0 {
		return strings.Fields(def)
	}
	return v
}

// visit fetches base once, and through it everything it references.
func (f *fetcher) visit(base string) {
	f.mu.Lock()
	if f.seen[base] {
		f.mu.Unlock()
		return
	}
	f.seen[base] = true
	f.mu.Unlock()
	f.g.Go(func() error {
		m, err := f.fetch(base)
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.done[base] = m
		f.mu.Unlock()
		return nil
	})
}

func (f *fetcher) fetch(base string) (*storeMeta, error) {
	dir := filepath.Join(f.b.opts.Artifacts, "store", "nix", "store")
	tarPath := filepath.Join(dir, base+".tar")
	metaPath := filepath.Join(dir, base+".json")
	var cached storeMeta
	if err := manifest.ReadJSON(metaPath, &cached); err == nil {
		if st, err := os.Stat(tarPath); err == nil && st.Size() == cached.Size {
			for _, r := range cached.References {
				f.visit(r)
			}
			return &cached, nil
		}
	}
	hash, _, _ := strings.Cut(base, "-")
	select {
	case f.narinfos <- struct{}{}:
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
	ni, sub, err := firstOf(f.ctx, f.subs, func(ctx context.Context, s string) (*nar.NarInfo, error) {
		return f.narinfo(ctx, s, hash)
	})
	<-f.narinfos
	if err != nil {
		return nil, fmt.Errorf("store path /nix/store/%s is not available from any substituter (%v); "+
			"it must be built and pushed to a cache first", base, err)
	}
	if ni.StorePath != "/nix/store/"+base {
		return nil, fmt.Errorf("narinfo mismatch: asked %s, got %s", base, ni.StorePath)
	}
	if err := ni.Verify(f.trusted); err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(ni.References))
	for _, r := range ni.References {
		if r != base {
			refs = append(refs, r)
			f.visit(r)
		}
	}
	sort.Strings(refs)
	want, err := nar.ParseSHA256(ni.NarHash)
	if err != nil {
		return nil, err
	}
	prio := ni.FileSize
	if prio == 0 {
		prio = ni.NarSize
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var bins []string
	var tarKey string
	var tarSize int64
	attempt := func() error {
		if err := f.nars.acquire(f.ctx, prio); err != nil {
			return err
		}
		s, err := f.openSpool(dir, sub, ni.URL, ni.FileSize)
		if err != nil {
			f.nars.release()
			return err
		}
		go func() {
			<-s.done
			f.nars.release()
		}()
		defer s.Close()
		if err := f.decoders.acquire(f.ctx, prio); err != nil {
			return err
		}
		defer f.decoders.release()
		bins, tarKey, tarSize, err = f.convert(s.Reader(), ni, base, dir, want)
		return err
	}
	// a NAR is downloaded and converted again when it does not match its
	// hash or the download failed for good; requests retry on their own.
	// A cache found to ignore Range is asked again at once, for the whole
	// file.
	err = retry(f.ctx, func() error {
		err := attempt()
		if errors.Is(err, errNoRange) {
			err = attempt()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	m := &storeMeta{
		StorePath: manifest.StorePath{
			Path:       ni.StorePath,
			NarHash:    ni.NarHash,
			Key:        tarKey,
			Size:       tarSize,
			References: refs,
		},
		Bins: bins,
	}
	if err := manifest.WriteJSON(metaPath, m); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.b.fetched++
	f.b.opts.Stats.FetchedPaths++
	f.b.opts.Stats.FetchedBytes += tarSize
	f.mu.Unlock()
	f.b.logf("  fetched %s (%s)", base, humanSize(tarSize))
	return m, nil
}

// convert decodes the compressed NAR in body and writes it as
// dir/base.tar, checking it against want. It returns the binaries in bin/ and the tar's digest and
// size.
func (f *fetcher) convert(body io.Reader, ni *nar.NarInfo, base, dir string, want []byte) (bins []string, key string, size int64, err error) {
	r := body
	switch ni.Compression {
	case "none", "":
	case "xz":
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, "", 0, err
		}
		r = xr
	case "zstd":
		zr, err := getZstd(r)
		if err != nil {
			return nil, "", 0, err
		}
		defer putZstd(zr)
		r = zr
	case "bzip2":
		r = bzip2.NewReader(r)
	default:
		return nil, "", 0, fmt.Errorf("%s: unsupported compression %q", base, ni.Compression)
	}
	tmp, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return nil, "", 0, err
	}
	defer os.Remove(tmp.Name())
	narHash := sha256.New()
	tarHash := sha256.New()
	cw := &countWriter{w: io.MultiWriter(tmp, tarHash)}
	tw := tar.NewWriter(cw)
	err = nar.ToTar(io.TeeReader(r, narHash), tw, "nix/store/"+base, func(e nar.Entry) {
		if d, file := path.Split(e.Path); d == "bin/" && e.Type != tar.TypeDir {
			bins = append(bins, file)
		}
	})
	if err == nil {
		// drain trailing bytes so the hash covers the whole NAR
		_, err = io.Copy(narHash, r)
	}
	if err == nil {
		err = tw.Close()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("convert %s: %w", base, err)
	}
	if got := narHash.Sum(nil); !bytes.Equal(got, want) {
		return nil, "", 0, fmt.Errorf("%s: NAR hash mismatch", base)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, base+".tar")); err != nil {
		return nil, "", 0, err
	}
	sort.Strings(bins)
	return bins, "sha256:" + hex.EncodeToString(tarHash.Sum(nil)), cw.n, nil
}

func (f *fetcher) narinfo(ctx context.Context, sub, hash string) (*nar.NarInfo, error) {
	resp, err := f.get(ctx, sub+"/"+hash+".narinfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return nar.ParseNarInfo(resp.Body)
}

// zstd decoders are reused: each holds a history window of up to several
// MB, and with many NARs converting at once fresh ones dominate memory.
// They decode in the calling goroutine; the parallelism is across NARs.
var zstdDecoders sync.Pool

func getZstd(r io.Reader) (*zstd.Decoder, error) {
	if d, ok := zstdDecoders.Get().(*zstd.Decoder); ok {
		if err := d.Reset(r); err == nil {
			return d, nil
		}
		d.Close()
	}
	return zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
}

func putZstd(d *zstd.Decoder) {
	if d.Reset(nil) == nil {
		zstdDecoders.Put(d)
	} else {
		d.Close()
	}
}
