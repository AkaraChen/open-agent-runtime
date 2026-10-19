// Package builder compiles a declarative oar.yaml into immutable artifacts:
// a flattened Debian rootfs, per-store-path Nix closure tarballs, imported
// component trees, and a bundle lock with all metadata the compositor needs.
package builder

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

// Options controls a build.
type Options struct {
	Artifacts string   // output directory
	Platforms []string // empty: all platforms declared in the config
	Update    bool     // re-resolve the base image tag
	Log       io.Writer
}

// Builder holds state for one build invocation.
type Builder struct {
	ctx  context.Context
	cfg  *spec.Config
	opts Options
	prev *manifest.Lock
}

// LockPath returns the lock location inside an artifacts dir.
func LockPath(artifacts string) string {
	return filepath.Join(artifacts, "manifests", "bundle.lock.json")
}

// Build runs the artifact builder and writes bundle.lock.json.
func Build(ctx context.Context, cfg *spec.Config, opts Options) (*manifest.Lock, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	b := &Builder{ctx: ctx, cfg: cfg, opts: opts}
	if prev, err := manifest.Read(LockPath(opts.Artifacts)); err == nil {
		b.prev = prev
	}
	platforms := opts.Platforms
	if len(platforms) == 0 {
		platforms = cfg.AllPlatforms()
	}
	indexDigest, err := b.resolveBaseDigest(cfg.Runtime.Base)
	if err != nil {
		return nil, err
	}
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
	for _, ps := range platforms {
		p, err := spec.ParsePlatform(ps)
		if err != nil {
			return nil, err
		}
		pl, err := b.buildPlatform(p, indexDigest)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		lock.Platforms[p.String()] = pl
	}
	if err := manifest.WriteJSON(LockPath(opts.Artifacts), lock); err != nil {
		return nil, err
	}
	return lock, nil
}

func (b *Builder) logf(format string, args ...any) {
	fmt.Fprintf(b.opts.Log, format+"\n", args...)
}

func (b *Builder) buildPlatform(p spec.Platform, indexDigest string) (*manifest.PlatformLock, error) {
	refs, err := b.cfg.ResolveComponents()
	if err != nil {
		return nil, err
	}
	base, err := b.buildBase(p, indexDigest)
	if err != nil {
		return nil, err
	}
	pl := &manifest.PlatformLock{
		Platform: p.String(),
		Base:     base.Base,
		Hostname: b.cfg.Runtime.Hostname,
	}
	if pl.Hostname == "" {
		pl.Hostname = "oar"
	}

	// Nix packages
	var attrs []string
	for _, r := range refs {
		if r.Kind == spec.KindNix {
			attrs = append(attrs, r.Name)
		}
	}
	var storeIndex map[string]*storeMeta
	if len(attrs) > 0 {
		evals, err := b.evalPackages(p, attrs)
		if err != nil {
			return nil, err
		}
		var roots []string
		for _, a := range attrs {
			roots = append(roots, evals[a].Outputs...)
		}
		b.logf("fetching runtime closure of %d packages for %s", len(attrs), p)
		storeIndex, err = b.fetchClosure(roots)
		if err != nil {
			return nil, err
		}
		for _, a := range attrs {
			ev := evals[a]
			pk := manifest.Package{Attr: a, OutPath: ev.Outputs[0], System: p.NixSystem()}
			for _, o := range ev.Outputs {
				pk.Bins = append(pk.Bins, storeIndex[path.Base(o)].Bins...)
			}
			sort.Strings(pk.Bins)
			pl.Packages = append(pl.Packages, pk)
		}
		var total int64
		for _, m := range storeIndex {
			pl.StorePaths = append(pl.StorePaths, m.StorePath)
			total += m.Size
		}
		b.logf("closure for %s: %d store paths, %s", p, len(storeIndex), humanSize(total))
	}

	// Imported components
	for _, r := range refs {
		if r.Kind == spec.KindNix {
			continue
		}
		c, err := b.buildComponent(r, p)
		if err != nil {
			return nil, err
		}
		pl.Components = append(pl.Components, *c)
	}

	pl.Normalize()
	warnings, err := checkConflicts(pl, base, storeIndex, p)
	if err != nil {
		return nil, err
	}
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
	for _, pk := range pl.Packages {
		for _, bin := range pk.Bins {
			if prev, ok := binOwner[bin]; ok {
				errs = append(errs, fmt.Sprintf("PATH conflict: %q is provided by both nix:%s and nix:%s", bin, prev, pk.Attr))
				continue
			}
			binOwner[bin] = pk.Attr
			for _, d := range []string{"usr/local/bin", "usr/bin", "bin", "usr/sbin", "sbin"} {
				if _, ok := base.Entries[d+"/"+bin]; ok {
					warnings = append(warnings, fmt.Sprintf("nix:%s/bin/%s shadows /%s/%s from the base (nix packages precede the base on PATH)", pk.Attr, bin, d, bin))
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
