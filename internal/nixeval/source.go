package nixeval

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
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

// extractTarGz writes a GitHub-style archive (one top-level directory) into
// dst without that directory. Entries that would land outside dst are
// refused. Symlinks are kept as they are: nothing here follows them.
func extractTarGz(r io.Reader, dst string) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		if h.Name == "pax_global_header" {
			continue
		}
		if path.IsAbs(h.Name) || slices.Contains(strings.Split(h.Name, "/"), "..") {
			return files, fmt.Errorf("archive entry %q escapes the destination", h.Name)
		}
		_, rel, ok := strings.Cut(path.Clean(h.Name), "/")
		if !ok || rel == "" || rel == "." {
			continue // the top-level directory itself
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return files, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return files, err
			}
			files++
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return files, err
			}
			files++
		default:
			// hard links and devices do not occur in a source archive
		}
	}
}
