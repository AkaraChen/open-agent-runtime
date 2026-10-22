package nixeval

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

// DefaultNixpkgsMirror serves `<mirror>/<rev>.tar.gz`; runtime.nixpkgs_mirror
// of the config overrides it.
const DefaultNixpkgsMirror = "https://github.com/NixOS/nixpkgs/archive"

// cacheRoot replaces CacheDir in tests.
var cacheRoot string

// CacheDir is where evaluation inputs live: nixpkgs checkouts and the
// embedded evaluator's compiled-file cache. They are build inputs shared by
// every project, not artifacts, so they stay out of the artifacts directory
// (a nixpkgs tree holds Go files that would break `go test ./...` of a
// module the artifacts sit in). It is in the user cache directory
// ($XDG_CACHE_HOME on Linux).
func CacheDir() string {
	if cacheRoot != "" {
		return cacheRoot
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "oar")
}

// SourceDir is where a revision's checkout lives.
func SourceDir(rev string) string {
	return filepath.Join(CacheDir(), "nixpkgs", rev)
}

// IRCacheDir is the embedded evaluator's compiled-file cache.
func IRCacheDir() string { return filepath.Join(CacheDir(), "iets") }

// Source returns the checkout named by ref: a directory as it is, or a
// nixpkgs commit downloaded once into SourceDir. The download is extracted
// next to its final place and renamed into it, so a directory that exists
// is complete.
func Source(ctx context.Context, ref, mirror string, log io.Writer) (string, error) {
	if log == nil {
		log = io.Discard
	}
	if spec.ValidateRevision(ref) != nil {
		dir, err := filepath.Abs(ref)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(dir, "default.nix")); err != nil {
			return "", fmt.Errorf("%s is neither a nixpkgs commit hash nor a nixpkgs checkout (no default.nix)", ref)
		}
		return dir, nil
	}
	dir := SourceDir(ref)
	if _, err := os.Stat(filepath.Join(dir, "default.nix")); err == nil {
		return dir, nil
	}
	if mirror == "" {
		mirror = DefaultNixpkgsMirror
	}
	url := strings.TrimSuffix(mirror, "/") + "/" + ref + ".tar.gz"
	fmt.Fprintf(log, "downloading nixpkgs@%s (once per revision)\n", ref[:12])
	start := time.Now()
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-"+ref[:12]+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download nixpkgs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download nixpkgs: GET %s: %s", url, resp.Status)
	}
	n, err := extractTarGz(resp.Body, tmp)
	if err != nil {
		return "", fmt.Errorf("extract nixpkgs: %w", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "default.nix")); err != nil {
		return "", fmt.Errorf("%s does not look like a nixpkgs archive (no default.nix at the top)", url)
	}
	if err := os.Rename(tmp, dir); err != nil {
		// another process finished first
		if _, serr := os.Stat(filepath.Join(dir, "default.nix")); serr == nil {
			return dir, nil
		}
		return "", err
	}
	fmt.Fprintf(log, "  %d files in %s\n", n, time.Since(start).Round(time.Millisecond))
	return dir, nil
}
