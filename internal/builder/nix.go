package builder

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

// DefaultSubstituter and DefaultTrustedKey describe the official cache.
const (
	DefaultSubstituter = "https://cache.nixos.org"
	DefaultTrustedKey  = "cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY="
)

// evalResult is the outcome of evaluating one attribute for one system.
type evalResult struct {
	Outputs     []string `json:"outputs"`
	MainProgram string   `json:"mainProgram,omitempty"`
}

type evalCache struct {
	Entries map[string]evalResult `json:"entries"`
}

func evalKey(rev, system, attr string) string { return rev + "/" + system + "/" + attr }

// evalPackages resolves attribute names to output store paths. Results are
// cached in artifacts/manifests/nix-eval.json; nix is only invoked for
// attributes that were never evaluated at this revision.
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
	quoted, _ := json.Marshal(missing)
	expr := fmt.Sprintf(`
let
  pkgs = import (builtins.fetchTarball "https://github.com/NixOS/nixpkgs/archive/%s.tar.gz") {
    system = %q; config = { allowUnfree = false; }; overlays = [];
  };
  lib = pkgs.lib;
  docs = [ "man" "doc" "devdoc" "info" ];
  get = n: let
      d = lib.getAttrFromPath (lib.splitString "." n) pkgs;
      outs = lib.subtractLists docs (d.meta.outputsToInstall or [ "out" ]);
    in {
      outputs = map (o: d.${o}.outPath) (if outs == [] then [ "out" ] else outs);
      mainProgram = d.meta.mainProgram or "";
    };
in builtins.listToAttrs (map (n: { name = n; value = get n; }) (builtins.fromJSON %q))
`, rev, system, string(quoted))
	res, err := b.runNixEval(expr)
	if err != nil {
		return nil, err
	}
	var parsed map[string]evalResult
	if err := json.Unmarshal(res, &parsed); err != nil {
		return nil, fmt.Errorf("parse nix eval output: %w\n%s", err, res)
	}
	for _, a := range missing {
		r, ok := parsed[a]
		if !ok || len(r.Outputs) == 0 {
			return nil, fmt.Errorf("nix eval: no outputs for %s", a)
		}
		out[a] = r
		cache.Entries[evalKey(rev, system, a)] = r
	}
	if err := manifest.WriteJSON(cachePath, &cache); err != nil {
		return nil, err
	}
	return out, nil
}

// runNixEval evaluates expr with a local nix-instantiate, or inside the
// nixos/nix container image when nix is not installed on the host.
func (b *Builder) runNixEval(expr string) ([]byte, error) {
	args := []string{"--eval", "--json", "--strict", "--read-write-mode", "--expr", expr}
	mode := os.Getenv("OAR_NIX")
	if mode == "" {
		if _, err := exec.LookPath("nix-instantiate"); err == nil {
			mode = "local"
		} else if _, err := exec.LookPath("docker"); err == nil {
			mode = "docker"
		} else {
			return nil, fmt.Errorf("nix evaluation requires nix-instantiate or docker on the build host")
		}
	}
	var cmd *exec.Cmd
	switch mode {
	case "local":
		cmd = exec.CommandContext(b.ctx, "nix-instantiate", args...)
	case "docker":
		img := os.Getenv("OAR_NIX_IMAGE")
		if img == "" {
			img = "nixos/nix:latest"
		}
		cmd = exec.CommandContext(b.ctx, "docker", append([]string{"run", "--rm", "-v", "oar-nix-cache:/root/.cache", img, "nix-instantiate"}, args...)...)
	default:
		return nil, fmt.Errorf("OAR_NIX=%s: want local or docker", mode)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nix eval (%s) failed: %w\n%s", mode, err, stderr.String())
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
	client  *http.Client
	subs    []string
	trusted []string
	mu      sync.Mutex
	done    map[string]*storeMeta
}

// fetchClosure fetches the runtime closure of roots (full store paths) from
// the binary cache and converts every path into a tar artifact.
func (b *Builder) fetchClosure(roots []string) (map[string]*storeMeta, error) {
	f := &fetcher{
		b:       b,
		client:  &http.Client{},
		subs:    splitEnv("OAR_NIX_SUBSTITUTERS", DefaultSubstituter),
		trusted: splitEnv("OAR_NIX_TRUSTED_KEYS", DefaultTrustedKey),
		done:    map[string]*storeMeta{},
	}
	queue := map[string]bool{}
	for _, r := range roots {
		queue[path.Base(r)] = true
	}
	for len(queue) > 0 {
		var batch []string
		for n := range queue {
			batch = append(batch, n)
		}
		sort.Strings(batch)
		queue = map[string]bool{}
		g, _ := errgroup.WithContext(b.ctx)
		g.SetLimit(8)
		results := make([]*storeMeta, len(batch))
		for i, n := range batch {
			i, n := i, n
			g.Go(func() error {
				m, err := f.fetch(n)
				results[i] = m
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
		for _, m := range results {
			f.done[m.Name()] = m
		}
		for _, m := range results {
			for _, ref := range m.References {
				if _, ok := f.done[ref]; !ok {
					queue[ref] = true
				}
			}
		}
	}
	return f.done, nil
}

func splitEnv(key, def string) []string {
	v := os.Getenv(key)
	if v == "" {
		v = def
	}
	return strings.Fields(v)
}

func (f *fetcher) fetch(base string) (*storeMeta, error) {
	dir := filepath.Join(f.b.opts.Artifacts, "store", "nix", "store")
	tarPath := filepath.Join(dir, base+".tar")
	metaPath := filepath.Join(dir, base+".json")
	var cached storeMeta
	if err := manifest.ReadJSON(metaPath, &cached); err == nil {
		if st, err := os.Stat(tarPath); err == nil && st.Size() == cached.Size {
			return &cached, nil
		}
	}
	hash, _, _ := strings.Cut(base, "-")
	var ni *nar.NarInfo
	var sub string
	var lastErr error
	for _, s := range f.subs {
		info, err := f.narinfo(s, hash)
		if err != nil {
			lastErr = err
			continue
		}
		ni, sub = info, s
		break
	}
	if ni == nil {
		return nil, fmt.Errorf("store path /nix/store/%s is not available from any substituter (%v); "+
			"it must be built and pushed to a cache first", base, lastErr)
	}
	if ni.StorePath != "/nix/store/"+base {
		return nil, fmt.Errorf("narinfo mismatch: asked %s, got %s", base, ni.StorePath)
	}
	if err := ni.Verify(f.trusted); err != nil {
		return nil, err
	}
	want, err := nar.ParseSHA256(ni.NarHash)
	if err != nil {
		return nil, err
	}
	resp, err := f.get(sub + "/" + ni.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body io.Reader = resp.Body
	switch ni.Compression {
	case "none", "":
	case "xz":
		xr, err := xz.NewReader(body)
		if err != nil {
			return nil, err
		}
		body = xr
	case "zstd":
		zr, err := zstd.NewReader(body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		body = zr
	case "bzip2":
		body = bzip2.NewReader(body)
	default:
		return nil, fmt.Errorf("%s: unsupported compression %q", base, ni.Compression)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	narHash := sha256.New()
	tarHash := sha256.New()
	cw := &countWriter{w: io.MultiWriter(tmp, tarHash)}
	tw := tar.NewWriter(cw)
	var bins []string
	err = nar.ToTar(io.TeeReader(body, narHash), tw, "nix/store/"+base, func(e nar.Entry) {
		if d, file := path.Split(e.Path); d == "bin/" && e.Type != tar.TypeDir {
			bins = append(bins, file)
		}
	})
	if err == nil {
		// drain trailing bytes so the hash covers the whole NAR
		_, err = io.Copy(narHash, body)
	}
	if err == nil {
		err = tw.Close()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("convert %s: %w", base, err)
	}
	if got := narHash.Sum(nil); !bytes.Equal(got, want) {
		return nil, fmt.Errorf("%s: NAR hash mismatch", base)
	}
	if err := os.Rename(tmp.Name(), tarPath); err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(ni.References))
	for _, r := range ni.References {
		if r != base {
			refs = append(refs, r)
		}
	}
	sort.Strings(refs)
	sort.Strings(bins)
	m := &storeMeta{
		StorePath: manifest.StorePath{
			Path:       ni.StorePath,
			NarHash:    ni.NarHash,
			Key:        "sha256:" + hex.EncodeToString(tarHash.Sum(nil)),
			Size:       cw.n,
			References: refs,
		},
		Bins: bins,
	}
	if err := manifest.WriteJSON(metaPath, m); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.b.fetched++
	f.mu.Unlock()
	f.b.logf("  fetched %s (%s)", base, humanSize(cw.n))
	return m, nil
}

func (f *fetcher) narinfo(sub, hash string) (*nar.NarInfo, error) {
	resp, err := f.get(sub + "/" + hash + ".narinfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return nar.ParseNarInfo(resp.Body)
}

func (f *fetcher) get(url string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(f.b.ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := f.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		resp.Body.Close()
		lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
		if resp.StatusCode == http.StatusNotFound {
			break
		}
	}
	return nil, lastErr
}
