// Package builder compiles a declarative oar.yaml into immutable artifacts:
// a flattened Debian rootfs, per-store-path Nix closure tarballs, imported
// component trees, and a bundle lock with all metadata the compositor needs.
package builder

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

// Options controls a build.
type Options struct {
	Artifacts string   // output directory
	Platforms []string // empty: all platforms declared in the config
	Update    bool     // re-resolve the base image tag
	Log       io.Writer
	// Stats, if non-nil, receives timing and transfer statistics.
	Stats *Stats
}

// Stats describes where a build spent its time and what it transferred.
type Stats struct {
	Total         time.Duration `json:"total"`
	Base          time.Duration `json:"base"`
	Eval          time.Duration `json:"eval"`
	Fetch         time.Duration `json:"fetch"`
	Components    time.Duration `json:"components"`
	Checks        time.Duration `json:"checks"`
	EvaluatedAttr int           `json:"evaluatedAttrs"`
	FetchedPaths  int           `json:"fetchedPaths"`
	FetchedBytes  int64         `json:"fetchedBytes"`
	ClosurePaths  int           `json:"closurePaths"`
	ClosureBytes  int64         `json:"closureBytes"`
}

// Builder holds state for one build invocation.
type Builder struct {
	ctx  context.Context
	cfg  *spec.Config
	opts Options
	prev *manifest.Lock
	// Platforms, and the stages of one platform, build concurrently on
	// copies of the Builder (each with its own cancellable ctx) that share
	// this state.
	*shared
}

type shared struct {
	// mu guards opts.Stats and fetched
	mu sync.Mutex
	// fetched counts store paths downloaded during this build
	fetched int
	// evalMu serializes evalPackages, which read-modify-writes nix-eval.json
	evalMu sync.Mutex
	// srcMu serializes user build commands (sources.*.build), which may
	// share output files between platforms
	srcMu sync.Mutex
	// baseDigest pins the index digest of the base image. The first base
	// stage to ask resolves it with the registry; the others get its answer,
	// so all platforms are built from the same index.
	baseDigest func() (string, error)
}

// stats runs fn with exclusive access to the build statistics.
func (b *Builder) stats(fn func(st *Stats)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(b.opts.Stats)
}

// syncWriter makes a log shared by concurrent stages safe to write to.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// LockPath returns the lock location inside an artifacts dir.
func LockPath(artifacts string) string {
	return filepath.Join(artifacts, "manifests", "bundle.lock.json")
}

// BundlesDir holds one immutable lock per compiled bundle digest.
func BundlesDir(artifacts string) string { return filepath.Join(artifacts, "manifests", "bundles") }

// BundlePath is the lock file of a compiled bundle.
func BundlePath(artifacts, digest string) string {
	_, h, ok := strings.Cut(digest, ":")
	if !ok {
		h = digest
	}
	return filepath.Join(BundlesDir(artifacts), h+".json")
}

// ConfigIndex maps one config file content to its compiled bundles.
type ConfigIndex struct {
	Config    string            `json:"config"`    // config hash
	Path      string            `json:"path"`      // where it was built from
	Platforms map[string]string `json:"platforms"` // platform -> bundle digest
}

func configIndexPath(artifacts, hash string) string {
	_, h, _ := strings.Cut(hash, ":")
	return filepath.Join(artifacts, "manifests", "configs", h+".json")
}

func recordConfig(artifacts string, cfg *spec.Config, lock *manifest.Lock) error {
	p := configIndexPath(artifacts, cfg.Hash)
	var idx ConfigIndex
	_ = manifest.ReadJSON(p, &idx)
	if idx.Platforms == nil {
		idx.Platforms = map[string]string{}
	}
	idx.Config, idx.Path = cfg.Hash, cfg.Dir
	for _, pl := range cfg.AllPlatforms() {
		if b, ok := lock.Platforms[pl]; ok {
			idx.Platforms[pl] = b.BundleDigest
		}
	}
	return manifest.WriteJSON(p, &idx)
}

// BundleForConfig returns the bundle compiled from exactly this config
// content for a platform.
func BundleForConfig(artifacts string, cfg *spec.Config, platform string) (*manifest.PlatformLock, error) {
	var idx ConfigIndex
	if err := manifest.ReadJSON(configIndexPath(artifacts, cfg.Hash), &idx); err != nil {
		return nil, fmt.Errorf("this config has not been compiled yet (run oar build)")
	}
	d, ok := idx.Platforms[platform]
	if !ok {
		return nil, fmt.Errorf("this config has not been compiled for %s (run oar build --platform %s)", platform, platform)
	}
	return FindBundle(artifacts, d)
}

// FindBundle loads a compiled bundle by full digest or unique hex prefix.
func FindBundle(artifacts, ref string) (*manifest.PlatformLock, error) {
	_, h, ok := strings.Cut(ref, ":")
	if !ok {
		h = ref
	}
	if len(h) < 6 {
		return nil, fmt.Errorf("bundle reference %q is too short (need at least 6 hex characters)", ref)
	}
	entries, err := os.ReadDir(BundlesDir(artifacts))
	if err != nil {
		return nil, fmt.Errorf("no compiled bundles in %s: %w", artifacts, err)
	}
	var match string
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if strings.HasPrefix(name, h) {
			if match != "" {
				return nil, fmt.Errorf("bundle reference %q is ambiguous", ref)
			}
			match = e.Name()
		}
	}
	if match == "" {
		return nil, fmt.Errorf("bundle %q not found in %s", ref, BundlesDir(artifacts))
	}
	var pl manifest.PlatformLock
	if err := manifest.ReadJSON(filepath.Join(BundlesDir(artifacts), match), &pl); err != nil {
		return nil, err
	}
	if got := pl.ComputeDigest(); got != pl.BundleDigest {
		return nil, fmt.Errorf("bundle %s is corrupt: content digest %s", pl.BundleDigest, got)
	}
	return &pl, nil
}

// Build runs the artifact builder and writes bundle.lock.json.
func Build(ctx context.Context, cfg *spec.Config, opts Options) (*manifest.Lock, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	opts.Log = &syncWriter{w: opts.Log}
	if opts.Stats == nil {
		opts.Stats = &Stats{}
	}
	start := time.Now()
	defer func() { opts.Stats.Total = time.Since(start) }()
	b := &Builder{ctx: ctx, cfg: cfg, opts: opts, shared: &shared{}}
	if prev, err := manifest.Read(LockPath(opts.Artifacts)); err == nil {
		b.prev = prev
	}
	platforms := opts.Platforms
	if len(platforms) == 0 {
		platforms = cfg.AllPlatforms()
	}
	// PERF: the digest used to be resolved here, before any platform started.
	// The nix stage (the nixpkgs download and the evaluation, the longest
	// chain of a build on a machine that never built) waited for a registry
	// round trip it has no use for: a ping, a token and a HEAD, 0.6 s from
	// Docker Hub on the machine this was measured on. Now the base stage,
	// which needs the digest, resolves it, beside the others, once for all
	// platforms.
	b.baseDigest = sync.OnceValues(func() (string, error) { return b.resolveBaseDigest(cfg.Runtime.Base) })
	lock := &manifest.Lock{
		Version:   manifest.LockVersion,
		Base:      cfg.Runtime.Base,
		Nixpkgs:   cfg.Runtime.Nixpkgs,
		Platforms: map[string]*manifest.PlatformLock{},
	}
	// keep other platforms from a previous lock with the same inputs
	if b.prev != nil && b.prev.Base == lock.Base && b.prev.Nixpkgs == lock.Nixpkgs {
		for k, v := range b.prev.Platforms {
			lock.Platforms[k] = v
		}
	}
	var parsed []spec.Platform
	for _, ps := range platforms {
		p, err := spec.ParsePlatform(ps)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, p)
	}
	// platforms share no inputs, so they build side by side
	var lockMu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for _, p := range parsed {
		g.Go(func() error {
			pb := *b
			pb.ctx = gctx
			pl, err := pb.buildPlatform(p)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			// every compiled bundle stays addressable by digest, so configs
			// can be switched (or shipped) without rebuilding
			if err := manifest.WriteJSON(BundlePath(opts.Artifacts, pl.BundleDigest), pl); err != nil {
				return err
			}
			lockMu.Lock()
			lock.Platforms[p.String()] = pl
			lockMu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := manifest.WriteJSON(LockPath(opts.Artifacts), lock); err != nil {
		return nil, err
	}
	if cfg.Hash != "" {
		if err := recordConfig(opts.Artifacts, cfg, lock); err != nil {
			return nil, err
		}
	}
	return lock, nil
}

func (b *Builder) logf(format string, args ...any) {
	fmt.Fprintf(b.opts.Log, format+"\n", args...)
}

func (b *Builder) buildPlatform(p spec.Platform) (*manifest.PlatformLock, error) {
	refs, err := b.cfg.ResolveComponents()
	if err != nil {
		return nil, err
	}
	pl := &manifest.PlatformLock{
		Platform: p.String(),
		Hostname: b.cfg.Runtime.Hostname,
	}
	if pl.Hostname == "" {
		pl.Hostname = "oar"
	}

	// The base image, the nix closure and the imported components do not
	// depend on each other: pull, evaluate and download, and import run
	// side by side.
	var (
		base       *BaseManifest
		storeIndex map[string]*storeMeta
		packages   []manifest.Package
		components = make([]*manifest.Component, len(refs))
	)
	g, ctx := errgroup.WithContext(b.ctx)
	pb := *b // per-platform view whose requests stop when a sibling stage fails
	pb.ctx = ctx
	g.Go(func() (err error) {
		t := time.Now()
		defer func() { b.stats(func(st *Stats) { st.Base += time.Since(t) }) }()
		indexDigest, err := pb.baseDigest()
		if err != nil {
			return err
		}
		base, err = pb.buildBase(p, indexDigest)
		return err
	})
	g.Go(func() (err error) {
		storeIndex, packages, err = pb.buildNix(p, refs)
		return err
	})
	g.Go(func() error {
		t := time.Now()
		defer func() { b.stats(func(st *Stats) { st.Components += time.Since(t) }) }()
		for i, r := range refs {
			if r.Kind == spec.KindNix {
				continue
			}
			c, err := pb.buildComponent(r, p)
			if err != nil {
				return err
			}
			components[i] = c
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	pl.Base = base.Base
	pl.Packages = packages
	for _, m := range storeIndex {
		pl.StorePaths = append(pl.StorePaths, m.StorePath)
	}
	for _, c := range components {
		if c != nil {
			pl.Components = append(pl.Components, *c)
		}
	}

	t := time.Now()
	pl.Normalize()
	warnings, err := checkConflicts(pl, base, storeIndex, p)
	if err != nil {
		return nil, err
	}
	defer func() { b.stats(func(st *Stats) { st.Checks += time.Since(t) }) }()
	pl.Warnings = warnings
	if err := b.resolveProcess(pl, base); err != nil {
		return nil, err
	}
	pl.Normalize()
	for _, w := range pl.Warnings {
		b.logf("warning: %s", w)
	}
	pl.BundleDigest = pl.ComputeDigest()
	return pl, nil
}

// buildNix evaluates the nix components (once per nixpkgs revision) and
// fetches their closure. It returns the closure and the resolved packages.
func (b *Builder) buildNix(p spec.Platform, refs []spec.Ref) (map[string]*storeMeta, []manifest.Package, error) {
	var nixRefs []spec.Ref
	byRev := map[string][]string{}
	for _, r := range refs {
		if r.Kind == spec.KindNix {
			nixRefs = append(nixRefs, r)
			rev := b.cfg.NixRevision(r)
			byRev[rev] = append(byRev[rev], r.Name)
		}
	}
	if len(nixRefs) == 0 {
		return nil, nil, nil
	}
	evals := map[string]evalResult{} // keyed by rev + "/" + attr
	t := time.Now()
	for _, rev := range sortedKeys(byRev) {
		res, err := b.evalPackages(p, rev, byRev[rev])
		if err != nil {
			return nil, nil, err
		}
		for a, r := range res {
			evals[rev+"/"+a] = r
		}
	}
	var roots []string
	for _, r := range nixRefs {
		roots = append(roots, evals[b.cfg.NixRevision(r)+"/"+r.Name].Outputs...)
	}
	b.stats(func(st *Stats) { st.Eval += time.Since(t) })
	t = time.Now()
	storeIndex, err := b.fetchClosure(roots)
	if err != nil {
		return nil, nil, err
	}
	b.stats(func(st *Stats) { st.Fetch += time.Since(t) })
	var packages []manifest.Package
	for _, r := range nixRefs {
		rev := b.cfg.NixRevision(r)
		ev := evals[rev+"/"+r.Name]
		pk := manifest.Package{Attr: r.Name, Nixpkgs: rev, OutPath: ev.Outputs[0], System: p.NixSystem()}
		for _, o := range ev.Outputs {
			pk.Bins = append(pk.Bins, storeIndex[path.Base(o)].Bins...)
		}
		sort.Strings(pk.Bins)
		packages = append(packages, pk)
	}
	var total int64
	for _, m := range storeIndex {
		total += m.Size
	}
	b.stats(func(st *Stats) {
		st.ClosurePaths += len(storeIndex)
		st.ClosureBytes += total
	})
	if n := b.fetchedCount(); n > 0 {
		b.logf("closure for %s: %d store paths, %s (%d fetched)", p, len(storeIndex), humanSize(total), n)
	}
	return storeIndex, packages, nil
}

func (b *Builder) fetchedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fetched
}

// checkConflicts detects file, binary and environment conflicts between
// components, nix packages and the base. Conflicts are errors: the result
// never depends on the order components are listed in.
func checkConflicts(pl *manifest.PlatformLock, base *BaseManifest, store map[string]*storeMeta, p spec.Platform) ([]string, error) {
	var warnings []string
	var errs []string

	// 1. file trees of imported components
	owner := map[string]string{} // path -> component
	isDir := map[string]bool{}
	for _, c := range pl.Components {
		for _, f := range c.Files {
			dir := strings.HasSuffix(f, "/")
			fp := strings.TrimSuffix(f, "/")
			for _, reserved := range []string{"nix", "proc", "sys", "dev"} {
				if (fp == reserved && !dir) || strings.HasPrefix(fp, reserved+"/") {
					errs = append(errs, fmt.Sprintf("component %s writes reserved path /%s", c.Name, fp))
				}
			}
			if prev, ok := owner[fp]; ok && prev != c.Name {
				if !(dir && isDir[fp]) {
					errs = append(errs, fmt.Sprintf("file conflict: /%s is provided by both %s and %s", fp, prev, c.Name))
				}
				continue
			}
			owner[fp], isDir[fp] = c.Name, dir
			// against the base rootfs
			if be, ok := base.Entries[fp]; ok {
				baseDir := be.Type == tar.TypeDir
				if dir && !baseDir {
					errs = append(errs, fmt.Sprintf("component %s: /%s is a directory but the base has a non-directory there", c.Name, fp))
				} else if !dir && !c.Override {
					errs = append(errs, fmt.Sprintf("component %s: /%s already exists in the base (set override: true to replace it)", c.Name, fp))
				}
			}
			// an ancestor that is a symlink in the base would be shadowed
			parts := strings.Split(fp, "/")
			for i := 1; i < len(parts); i++ {
				anc := strings.Join(parts[:i], "/")
				if be, ok := base.Entries[anc]; ok && be.Type == tar.TypeSymlink {
					errs = append(errs, fmt.Sprintf("component %s: /%s would shadow base symlink /%s -> %s (use the resolved path)", c.Name, fp, anc, be.Link))
					break
				}
			}
		}
		// 2. ABI constraints of ELF files
		for _, e := range c.ELF {
			if e.Machine != p.Arch {
				errs = append(errs, fmt.Sprintf("component %s: %s is built for %s, target is %s", c.Name, e.Path, e.Machine, p.Arch))
			}
			if e.Interpreter == "" {
				continue
			}
			if strings.HasPrefix(e.Interpreter, "/nix/store/") {
				name := strings.SplitN(strings.TrimPrefix(e.Interpreter, "/nix/store/"), "/", 2)[0]
				if _, ok := store[name]; !ok {
					errs = append(errs, fmt.Sprintf("component %s: %s needs interpreter %s which is not in the nix closure", c.Name, e.Path, e.Interpreter))
				}
			} else if _, ok := resolveIn(base.Entries, e.Interpreter); !ok {
				errs = append(errs, fmt.Sprintf("component %s: %s needs interpreter %s which the base does not provide", c.Name, e.Path, e.Interpreter))
			} else {
				for _, lib := range e.Needed {
					if !baseHasLib(base, lib) && !owned(owner, lib) {
						warnings = append(warnings, fmt.Sprintf("component %s: %s needs %s which was not found in the base library paths", c.Name, e.Path, lib))
					}
				}
			}
		}
	}

	// 3. binaries on PATH from different nix packages
	binOwner := map[string]string{}
	label := func(pk manifest.Package) string {
		if pk.Nixpkgs != "" {
			return "nix:" + pk.Attr + "@" + pk.Nixpkgs[:min(12, len(pk.Nixpkgs))]
		}
		return "nix:" + pk.Attr
	}
	for _, pk := range pl.Packages {
		for _, bin := range pk.Bins {
			if prev, ok := binOwner[bin]; ok {
				errs = append(errs, fmt.Sprintf("PATH conflict: %q is provided by both %s and %s", bin, prev, label(pk)))
				continue
			}
			binOwner[bin] = label(pk)
			for _, d := range []string{"usr/local/bin", "usr/bin", "bin", "usr/sbin", "sbin"} {
				if _, ok := base.Entries[d+"/"+bin]; ok {
					warnings = append(warnings, fmt.Sprintf("%s/bin/%s shadows /%s/%s from the base (nix packages precede the base on PATH)", label(pk), bin, d, bin))
					break
				}
			}
		}
	}

	// 4. environment variables from components
	envOwner := map[string][2]string{}
	for _, c := range pl.Components {
		for k, v := range c.Env {
			if prev, ok := envOwner[k]; ok && prev[1] != v {
				errs = append(errs, fmt.Sprintf("env conflict: %s set to %q by %s and %q by %s", k, prev[1], prev[0], v, c.Name))
				continue
			}
			envOwner[k] = [2]string{c.Name, v}
		}
	}
	sort.Strings(errs)
	if len(errs) > 0 {
		return nil, fmt.Errorf("conflicts detected:\n  %s", strings.Join(errs, "\n  "))
	}
	return warnings, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func baseHasLib(base *BaseManifest, lib string) bool {
	for _, d := range []string{"lib", "usr/lib", "lib64", "usr/lib64",
		"lib/x86_64-linux-gnu", "usr/lib/x86_64-linux-gnu", "lib/aarch64-linux-gnu", "usr/lib/aarch64-linux-gnu"} {
		if _, ok := resolveIn(base.Entries, "/"+d+"/"+lib); ok {
			return true
		}
	}
	return false
}

func owned(owner map[string]string, lib string) bool {
	for p := range owner {
		if path.Base(p) == lib {
			return true
		}
	}
	return false
}

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// resolveProcess computes argv, PATH and the environment.
func (b *Builder) resolveProcess(pl *manifest.PlatformLock, base *BaseManifest) error {
	pc := b.cfg.Process
	proc := manifest.Process{Args: pc.Command, Cwd: pc.Cwd}
	if proc.Cwd == "" {
		proc.Cwd = "/"
	}
	if pc.User != "" {
		u, g, _ := strings.Cut(pc.User, ":")
		uid, err := strconv.ParseUint(u, 10, 32)
		if err != nil {
			return fmt.Errorf("process.user must be numeric uid[:gid], got %q", pc.User)
		}
		proc.UID, proc.GID = uint32(uid), uint32(uid)
		if g != "" {
			gid, err := strconv.ParseUint(g, 10, 32)
			if err != nil {
				return fmt.Errorf("process.user must be numeric uid[:gid], got %q", pc.User)
			}
			proc.GID = uint32(gid)
		}
	}

	// PATH: nix packages (sorted by attr), component entries (sorted), base
	var pathParts []string
	for _, pk := range pl.Packages {
		if len(pk.Bins) > 0 {
			pathParts = append(pathParts, pk.OutPath+"/bin")
		}
	}
	var compPaths []string
	for _, c := range pl.Components {
		compPaths = append(compPaths, c.PathEntries...)
	}
	sort.Strings(compPaths)
	pathParts = append(pathParts, compPaths...)
	basePath := defaultPath
	env := map[string]string{"HOME": "/root", "TERM": "xterm"}
	for _, kv := range base.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if k == "PATH" {
				basePath = v
			} else {
				env[k] = v
			}
		}
	}
	pathParts = append(pathParts, basePath)
	env["PATH"] = strings.Join(pathParts, ":")
	for _, c := range pl.Components {
		for k, v := range c.Env {
			env[k] = v
		}
	}
	for k, v := range pc.Env {
		env[k] = v
	}
	for k, v := range env {
		proc.Env = append(proc.Env, k+"="+v)
	}
	sort.Strings(proc.Env)

	// entry point must exist somewhere in the composed view
	if len(proc.Args) > 0 && path.IsAbs(proc.Args[0]) {
		if !entryExists(pl, base, proc.Args[0]) {
			return fmt.Errorf("process.command[0] %s does not exist in the composed rootfs", proc.Args[0])
		}
	}
	pl.Process = proc
	if r := pc.Ready; r != nil {
		pl.Ready = &manifest.Ready{TCP: r.TCP, Exec: r.Exec, Log: r.Log, Timeout: r.Timeout}
	}
	for _, p := range pc.Ports {
		pl.Ports = append(pl.Ports, manifest.Port{Name: p.Name, Port: p.Port, Env: p.Env})
	}
	pl.Snapshot = pc.Snapshot
	return nil
}

func entryExists(pl *manifest.PlatformLock, base *BaseManifest, p string) bool {
	clean := cleanPath(p)
	if strings.HasPrefix(clean, "nix/store/") {
		name := strings.SplitN(strings.TrimPrefix(clean, "nix/store/"), "/", 2)[0]
		for _, sp := range pl.StorePaths {
			if sp.Name() == name {
				return true
			}
		}
		return false
	}
	for _, c := range pl.Components {
		for _, f := range c.Files {
			if f == clean {
				return true
			}
		}
	}
	_, ok := resolveIn(base.Entries, p)
	return ok
}
