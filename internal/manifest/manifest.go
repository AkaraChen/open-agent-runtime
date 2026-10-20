// Package manifest defines the immutable metadata produced by the artifact
// builder and consumed by the compositor.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LockVersion is bumped whenever the lock schema changes incompatibly.
const LockVersion = 1

// Lock is artifacts/manifests/bundle.lock.json.
type Lock struct {
	Version   int                      `json:"version"`
	Base      string                   `json:"base"`
	Nixpkgs   string                   `json:"nixpkgs"`
	Platforms map[string]*PlatformLock `json:"platforms"`
}

// PlatformLock is everything needed to compose one bundle for one platform.
type PlatformLock struct {
	Platform     string      `json:"platform"`
	BundleDigest string      `json:"bundleDigest"`
	Base         Base        `json:"base"`
	Packages     []Package   `json:"packages"`
	StorePaths   []StorePath `json:"storePaths"`
	Components   []Component `json:"components"`
	Process      Process     `json:"process"`
	Hostname     string      `json:"hostname"`
	// Ready is a lifecycle readiness check; it does not change what the
	// container observes and is therefore not part of the bundle digest.
	Ready *Ready `json:"ready,omitempty"`
	// Ports are served by the ingress proxy (lifecycle, not in the digest).
	Ports    []Port   `json:"ports,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Port is an ingress port.
type Port struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Env  string `json:"env"`
}

// Ready is a readiness check.
type Ready struct {
	TCP     string        `json:"tcp,omitempty"`
	Exec    []string      `json:"exec,omitempty"`
	Log     string        `json:"log,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Base is a flattened OCI image root filesystem.
type Base struct {
	Ref         string `json:"ref"`
	IndexDigest string `json:"indexDigest,omitempty"`
	Digest      string `json:"digest"` // platform manifest digest
	Key         string `json:"key"`    // sha256 of rootfs.tar
	Size        int64  `json:"size"`
	Path        string `json:"path"` // relative to artifacts dir
	// Interpreters lists ELF program interpreters available in the base.
	Interpreters []string `json:"interpreters,omitempty"`
}

// Package is a top-level nix package requested by a component reference.
type Package struct {
	Attr    string            `json:"attr"`
	Nixpkgs string            `json:"nixpkgs,omitempty"` // revision it was evaluated at
	OutPath string            `json:"outPath"`
	System  string            `json:"system"`
	Bins    []string          `json:"bins"`
	Env     map[string]string `json:"env,omitempty"`
}

// StorePath is a single /nix/store path in the runtime closure.
type StorePath struct {
	Path       string   `json:"path"`
	NarHash    string   `json:"narHash"`
	Key        string   `json:"key"` // sha256 of the tar artifact
	Size       int64    `json:"size"`
	References []string `json:"references,omitempty"`
}

// Name is the store path basename (hash-name).
func (s StorePath) Name() string { return filepath.Base(s.Path) }

// Component is an imported binary, directory or file tree.
type Component struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Key         string            `json:"key"` // sha256 of tree.tar
	Size        int64             `json:"size"`
	Path        string            `json:"path"` // relative to artifacts dir
	Target      string            `json:"target"`
	Files       []string          `json:"files"`
	Env         map[string]string `json:"env,omitempty"`
	PathEntries []string          `json:"pathEntries,omitempty"`
	Override    bool              `json:"override,omitempty"`
	ELF         []ELFInfo         `json:"elf,omitempty"`
}

// ELFInfo records ABI constraints of an ELF file inside a component.
type ELFInfo struct {
	Path        string   `json:"path"`
	Machine     string   `json:"machine"`
	Interpreter string   `json:"interpreter,omitempty"`
	Needed      []string `json:"needed,omitempty"`
	Static      bool     `json:"static"`
}

// Process is the fully resolved container entry point.
type Process struct {
	Args []string `json:"args"`
	Env  []string `json:"env"` // sorted KEY=VALUE
	Cwd  string   `json:"cwd"`
	UID  uint32   `json:"uid"`
	GID  uint32   `json:"gid"`
}

// digestInput is the order-independent subset of a PlatformLock that
// determines what the container observes.
type digestInput struct {
	Platform   string     `json:"platform"`
	Base       [2]string  `json:"base"`
	Packages   [][]string `json:"packages"`
	StorePaths [][]string `json:"storePaths"`
	Components [][]string `json:"components"`
	Process    Process    `json:"process"`
	Hostname   string     `json:"hostname"`
}

// ComputeDigest returns the deterministic bundle digest. All collections are
// sorted first, so the declaration order of components never matters.
func (p *PlatformLock) ComputeDigest() string {
	in := digestInput{
		Platform: p.Platform,
		Base:     [2]string{p.Base.Digest, p.Base.Key},
		Process:  p.Process,
		Hostname: p.Hostname,
	}
	for _, pk := range p.Packages {
		in.Packages = append(in.Packages, []string{pk.Attr, pk.OutPath})
	}
	for _, sp := range p.StorePaths {
		in.StorePaths = append(in.StorePaths, []string{sp.Path, sp.NarHash, sp.Key})
	}
	for _, c := range p.Components {
		in.Components = append(in.Components, []string{c.Kind, c.Name, c.Key, c.Target, fmt.Sprint(c.Override)})
	}
	sortRows(in.Packages)
	sortRows(in.StorePaths)
	sortRows(in.Components)
	in.Process.Env = append([]string(nil), in.Process.Env...)
	sort.Strings(in.Process.Env)
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sortRows(rows [][]string) {
	sort.Slice(rows, func(i, j int) bool {
		return strings.Join(rows[i], "\x00") < strings.Join(rows[j], "\x00")
	})
}

// Normalize sorts every collection in canonical order.
func (p *PlatformLock) Normalize() {
	sort.Slice(p.Packages, func(i, j int) bool {
		if p.Packages[i].Attr != p.Packages[j].Attr {
			return p.Packages[i].Attr < p.Packages[j].Attr
		}
		return p.Packages[i].Nixpkgs < p.Packages[j].Nixpkgs
	})
	sort.Slice(p.StorePaths, func(i, j int) bool { return p.StorePaths[i].Path < p.StorePaths[j].Path })
	sort.Slice(p.Components, func(i, j int) bool {
		return p.Components[i].Kind+":"+p.Components[i].Name < p.Components[j].Kind+":"+p.Components[j].Name
	})
	sort.Strings(p.Process.Env)
	sort.Strings(p.Warnings)
}

// Read loads a lock file.
func Read(path string) (*Lock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if l.Version != LockVersion {
		return nil, fmt.Errorf("%s: unsupported lock version %d", path, l.Version)
	}
	return &l, nil
}

// WriteJSON atomically writes v as indented JSON.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadJSON decodes a JSON file into v.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
