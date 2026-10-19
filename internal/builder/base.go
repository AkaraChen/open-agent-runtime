package builder

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

// FSEntry is a compact description of a filesystem object, used for
// conflict detection and ABI checks.
type FSEntry struct {
	Type byte   `json:"t"`
	Link string `json:"l,omitempty"`
}

// BaseManifest is written next to rootfs.tar.
type BaseManifest struct {
	manifest.Base
	Platform string             `json:"platform"`
	Entries  map[string]FSEntry `json:"entries"`
	Env      []string           `json:"env,omitempty"`
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func baseSlug(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	return strings.Trim(slugRe.ReplaceAllString(ref, "-"), "-")
}

// resolveBaseDigest returns the pinned index digest for ref.
func (b *Builder) resolveBaseDigest(ref string) (string, error) {
	if _, d, ok := strings.Cut(ref, "@"); ok {
		return d, nil
	}
	if !b.opts.Update && b.prev != nil && b.prev.Base == ref {
		for _, pl := range b.prev.Platforms {
			if pl.Base.IndexDigest != "" {
				return pl.Base.IndexDigest, nil
			}
		}
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(r, remote.WithContext(b.ctx))
	if err != nil {
		d2, err2 := remote.Get(r, remote.WithContext(b.ctx))
		if err2 != nil {
			return "", fmt.Errorf("resolve %s: %w", ref, err)
		}
		return d2.Digest.String(), nil
	}
	return desc.Digest.String(), nil
}

func (b *Builder) buildBase(p spec.Platform, indexDigest string) (*BaseManifest, error) {
	ref := b.cfg.Runtime.Base
	repo, _, _ := strings.Cut(ref, "@")
	slug := baseSlug(repo)
	dir := filepath.Join(b.opts.Artifacts, "bases", slug, p.Slug())
	mfPath := filepath.Join(dir, "manifest.json")

	var cached BaseManifest
	if err := manifest.ReadJSON(mfPath, &cached); err == nil && cached.IndexDigest == indexDigest {
		if st, err := os.Stat(filepath.Join(dir, "rootfs.tar")); err == nil && st.Size() == cached.Size {
			return &cached, nil
		}
	}

	tagged, err := name.ParseReference(repo)
	if err != nil {
		return nil, err
	}
	r, err := name.NewDigest(tagged.Context().Name() + "@" + indexDigest)
	if err != nil {
		return nil, err
	}
	b.logf("pulling base %s@%s for %s", repo, short(indexDigest), p)
	img, err := remote.Image(r, remote.WithContext(b.ctx), remote.WithPlatform(v1.Platform{OS: p.OS, Architecture: p.Arch}))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	if cf.Architecture != p.Arch {
		return nil, fmt.Errorf("base %s: got architecture %s, want %s", ref, cf.Architecture, p.Arch)
	}
	dg, err := img.Digest()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, "rootfs.tar.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	rc := mutate.Extract(img)
	defer rc.Close()

	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	tw := tar.NewWriter(cw)
	tr := tar.NewReader(rc)
	entries := map[string]FSEntry{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("extract base: %w", err)
		}
		clean := cleanPath(hdr.Name)
		if clean == "" {
			continue
		}
		hdr.Name = clean
		if hdr.Typeflag == tar.TypeDir {
			hdr.Name += "/"
		}
		if hdr.Typeflag == tar.TypeLink {
			hdr.Linkname = cleanPath(hdr.Linkname)
		}
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			f.Close()
			return nil, err
		}
		entries[clean] = FSEntry{Type: hdr.Typeflag, Link: hdr.Linkname}
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "rootfs.tar")); err != nil {
		return nil, err
	}
	m := &BaseManifest{
		Base: manifest.Base{
			Ref:         ref,
			IndexDigest: indexDigest,
			Digest:      dg.String(),
			Key:         "sha256:" + hex.EncodeToString(h.Sum(nil)),
			Size:        cw.n,
			Path:        filepath.ToSlash(filepath.Join("bases", slug, p.Slug(), "rootfs.tar")),
		},
		Platform: p.String(),
		Entries:  entries,
		Env:      cf.Config.Env,
	}
	m.Interpreters = findInterpreters(entries)
	if err := manifest.WriteJSON(mfPath, m); err != nil {
		return nil, err
	}
	b.logf("base %s (%s): %d entries, %s", ref, p, len(entries), humanSize(cw.n))
	return m, nil
}

func findInterpreters(entries map[string]FSEntry) []string {
	var out []string
	for _, cand := range []string{
		"/lib/ld-linux-aarch64.so.1", "/lib64/ld-linux-x86-64.so.2",
		"/lib/ld-linux-x86-64.so.2", "/lib/ld-musl-aarch64.so.1", "/lib/ld-musl-x86_64.so.1",
	} {
		if _, ok := resolveIn(entries, cand); ok {
			out = append(out, cand)
		}
	}
	sort.Strings(out)
	return out
}

// resolveIn follows symlinks inside an entries map and reports whether the
// absolute path p resolves to an existing non-directory object.
func resolveIn(entries map[string]FSEntry, p string) (string, bool) {
	cur := cleanPath(p)
	for hops := 0; hops < 40; hops++ {
		parts := strings.Split(cur, "/")
		resolved := ""
		restart := false
		for i, part := range parts {
			next := path.Join(resolved, part)
			e, ok := entries[next]
			if !ok {
				return "", false
			}
			if e.Type == tar.TypeSymlink {
				target := e.Link
				if !strings.HasPrefix(target, "/") {
					target = path.Join(path.Dir("/"+next), target)
				}
				rest := strings.Join(parts[i+1:], "/")
				cur = cleanPath(path.Join(target, rest))
				restart = true
				break
			}
			resolved = next
		}
		if !restart {
			e := entries[resolved]
			return resolved, e.Type != tar.TypeDir
		}
	}
	return "", false
}

func cleanPath(p string) string {
	p = path.Clean("/" + strings.TrimPrefix(p, "./"))
	return strings.TrimPrefix(p, "/")
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func short(d string) string {
	_, h, _ := strings.Cut(d, ":")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
