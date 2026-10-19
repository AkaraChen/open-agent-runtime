// Package spec defines the declarative runtime configuration (oar.yaml).
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultNixpkgsRevision is the pinned nixpkgs revision (nixos-26.05) used
// when the configuration does not specify one.
const DefaultNixpkgsRevision = "8ce73368d1efd4d657e9064d29b144a52046b4e8"

// Config is the top-level oar.yaml document.
type Config struct {
	Runtime    Runtime            `yaml:"runtime"`
	Components []string           `yaml:"components"`
	Sources    map[string]*Source `yaml:"sources"`
	Process    Process            `yaml:"process"`

	// Dir is the directory containing the config file; relative paths in
	// sources are resolved against it.
	Dir string `yaml:"-"`
}

// Runtime selects the base image and target platforms.
type Runtime struct {
	Base      string   `yaml:"base"`
	Platform  string   `yaml:"platform"`
	Platforms []string `yaml:"platforms"`
	Nixpkgs   string   `yaml:"nixpkgs"`
	Hostname  string   `yaml:"hostname"`
}

// Source defines where a non-nix component comes from.
type Source struct {
	// Path to a file or directory. May contain {{arch}} / {{os}} placeholders.
	Path string `yaml:"path"`
	// Content is the literal file content, used instead of Path for file
	// and binary components. It is written byte for byte (no placeholder
	// expansion).
	Content *string `yaml:"content"`
	// Target is the absolute path inside the container. Defaults to
	// /opt/<name> for binaries and files, and /opt/<name>/ for directories.
	Target string `yaml:"target"`
	// Mode overrides the file mode for binary/file components (octal string).
	Mode string `yaml:"mode"`
	// Build is an optional command executed (in Dir) before import, with
	// {{arch}} / {{os}} placeholders expanded and OAR_ARCH/OAR_OS set.
	Build []string `yaml:"build"`
	// BuildEnv is added to the build command environment (placeholders
	// are expanded).
	BuildEnv map[string]string `yaml:"build_env"`
	// Env contributed to the container process.
	Env map[string]string `yaml:"env"`
	// Path entries contributed to PATH (absolute container paths).
	PathEntries []string `yaml:"path_entries"`
	// Requires lists other components this one depends on.
	Requires []string `yaml:"requires"`
	// Override allows this component to replace files of the base rootfs.
	Override bool `yaml:"override"`
}

// Process describes the container entry point.
type Process struct {
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
	Cwd     string            `yaml:"cwd"`
	User    string            `yaml:"user"`
}

// Kind of a component reference.
type Kind string

const (
	KindNix    Kind = "nix"
	KindBinary Kind = "binary"
	KindDir    Kind = "dir"
	KindFile   Kind = "file"
)

// Ref is a parsed component reference such as "nix:nodejs_22" or
// "nix:nodejs_22@<nixpkgs commit>".
type Ref struct {
	Kind Kind
	Name string
	// Rev pins a nix package to a nixpkgs revision other than
	// runtime.nixpkgs. Empty means the global revision.
	Rev string
}

func (r Ref) String() string {
	if r.Rev != "" {
		return string(r.Kind) + ":" + r.Name + "@" + r.Rev
	}
	return string(r.Kind) + ":" + r.Name
}

var revRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidateRevision requires a full nixpkgs commit hash so builds are
// reproducible (branch names and tags move).
func ValidateRevision(rev string) error {
	if !revRe.MatchString(rev) {
		return fmt.Errorf("nixpkgs revision %q must be a full 40-character commit hash", rev)
	}
	return nil
}

// ParseRef parses "kind:name" (and "nix:attr@rev").
func ParseRef(s string) (Ref, error) {
	k, n, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || n == "" {
		return Ref{}, fmt.Errorf("invalid component %q: expected kind:name", s)
	}
	switch Kind(k) {
	case KindNix, KindBinary, KindDir, KindFile:
	default:
		return Ref{}, fmt.Errorf("invalid component %q: unknown kind %q (want nix, binary, dir, file)", s, k)
	}
	r := Ref{Kind: Kind(k), Name: n}
	if name, rev, ok := strings.Cut(n, "@"); ok {
		if r.Kind != KindNix {
			return Ref{}, fmt.Errorf("invalid component %q: @revision is only supported for nix components", s)
		}
		if name == "" {
			return Ref{}, fmt.Errorf("invalid component %q: missing attribute name", s)
		}
		if err := ValidateRevision(rev); err != nil {
			return Ref{}, fmt.Errorf("invalid component %q: %w", s, err)
		}
		r.Name, r.Rev = name, rev
	}
	return r, nil
}

// NixRevision returns the nixpkgs revision used for a nix component.
func (c *Config) NixRevision(r Ref) string {
	if r.Rev != "" {
		return r.Rev
	}
	return c.Runtime.Nixpkgs
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.Dir = filepath.Dir(abs)
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks structural constraints.
func (c *Config) Validate() error {
	if c.Runtime.Base == "" {
		return fmt.Errorf("runtime.base is required")
	}
	if c.Runtime.Nixpkgs == "" {
		c.Runtime.Nixpkgs = DefaultNixpkgsRevision
	}
	if err := ValidateRevision(c.Runtime.Nixpkgs); err != nil {
		return fmt.Errorf("runtime.nixpkgs: %w", err)
	}
	if len(c.Process.Command) == 0 {
		return fmt.Errorf("process.command is required")
	}
	for _, p := range c.AllPlatforms() {
		if _, err := ParsePlatform(p); err != nil {
			return err
		}
	}
	if _, err := c.ResolveComponents(); err != nil {
		return err
	}
	return nil
}

// AllPlatforms returns every platform the config declares (sorted, unique).
func (c *Config) AllPlatforms() []string {
	set := map[string]bool{}
	if c.Runtime.Platform != "" {
		set[c.Runtime.Platform] = true
	}
	for _, p := range c.Runtime.Platforms {
		set[p] = true
	}
	if len(set) == 0 {
		set[HostPlatform()] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// DefaultPlatform is runtime.platform, or the host-native linux platform.
func (c *Config) DefaultPlatform() string {
	if c.Runtime.Platform != "" {
		return c.Runtime.Platform
	}
	return HostPlatform()
}

// ResolveComponents parses the unordered component set, expands `requires`
// transitively and returns a de-duplicated, sorted list. The result does not
// depend on the order components were declared in.
func (c *Config) ResolveComponents() ([]Ref, error) {
	seen := map[string]Ref{}
	var visit func(s string, stack []string) error
	visit = func(s string, stack []string) error {
		r, err := ParseRef(s)
		if err != nil {
			return err
		}
		key := r.String()
		for _, st := range stack {
			if st == key {
				return fmt.Errorf("dependency cycle: %s -> %s", strings.Join(stack, " -> "), key)
			}
		}
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = r
		if r.Kind == KindNix {
			return nil
		}
		src, ok := c.Sources[r.Name]
		if !ok {
			return fmt.Errorf("component %s has no entry in sources", key)
		}
		switch {
		case src.Content != nil && src.Path != "":
			return fmt.Errorf("sources.%s: set either path or content, not both", r.Name)
		case src.Content != nil && r.Kind == KindDir:
			return fmt.Errorf("sources.%s: content is only supported for file: and binary: components", r.Name)
		case src.Content != nil && len(src.Build) > 0:
			return fmt.Errorf("sources.%s: build is not supported with inline content", r.Name)
		case src.Content == nil && src.Path == "":
			return fmt.Errorf("sources.%s needs path or content", r.Name)
		}
		for _, dep := range src.Requires {
			if err := visit(dep, append(stack, key)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, s := range c.Components {
		if err := visit(s, nil); err != nil {
			return nil, err
		}
	}
	out := make([]Ref, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// Platform is an OS/arch pair, e.g. linux/arm64.
type Platform struct {
	OS   string
	Arch string
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Slug is a filesystem-friendly name, e.g. linux-arm64.
func (p Platform) Slug() string { return p.OS + "-" + p.Arch }

// NixSystem maps to a nix system double, e.g. aarch64-linux.
func (p Platform) NixSystem() string {
	switch p.Arch {
	case "amd64":
		return "x86_64-linux"
	case "arm64":
		return "aarch64-linux"
	}
	return p.Arch + "-linux"
}

// ParsePlatform parses "linux/amd64" or "linux/arm64".
func ParsePlatform(s string) (Platform, error) {
	o, a, ok := strings.Cut(s, "/")
	if !ok || o != "linux" || (a != "amd64" && a != "arm64") {
		return Platform{}, fmt.Errorf("unsupported platform %q (want linux/amd64 or linux/arm64)", s)
	}
	return Platform{OS: o, Arch: a}, nil
}

// HostPlatform is the linux platform matching the host CPU.
func HostPlatform() string {
	if runtime.GOARCH == "arm64" {
		return "linux/arm64"
	}
	return "linux/amd64"
}

// Expand replaces {{arch}} and {{os}} placeholders.
func Expand(s string, p Platform) string {
	return strings.NewReplacer("{{arch}}", p.Arch, "{{os}}", p.OS).Replace(s)
}
