// Package compose turns a bundle lock plus warm guest caches into an OCI
// bundle description. It is the hot path: it never evaluates Nix, touches
// the network, installs packages or copies file contents. Its only outputs
// are a small config.json and the ordered list of overlay layers.
package compose

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
)

// Layout describes the guest cache directory.
type Layout struct{ Root string }

func hexOf(key string) string {
	_, h, ok := strings.Cut(key, ":")
	if !ok {
		return key
	}
	return h
}

// BaseDir is the extracted base rootfs for a base key.
func (l Layout) BaseDir(key string) string {
	return filepath.Join(l.Root, "bases", hexOf(key), "rootfs")
}

// StoreRoot is the shared immutable /nix/store.
func (l Layout) StoreRoot() string { return filepath.Join(l.Root, "store", "nix", "store") }

// StorePath is a single extracted store path.
func (l Layout) StorePath(name string) string { return filepath.Join(l.StoreRoot(), name) }

// ComponentDir is an extracted component tree.
func (l Layout) ComponentDir(key string) string {
	return filepath.Join(l.Root, "components", hexOf(key), "tree")
}

// BundleDir caches composed bundle templates by digest.
func (l Layout) BundleDir(digest string) string {
	return filepath.Join(l.Root, "bundles", hexOf(digest))
}

// InstancesDir holds per-container instance directories.
func (l Layout) InstancesDir() string { return filepath.Join(l.Root, "instances") }

// Tmp is a scratch directory on the same filesystem as the caches.
func (l Layout) Tmp() string { return filepath.Join(l.Root, "tmp") }

// Bundle is a cached, composed bundle template.
type Bundle struct {
	Digest string `json:"digest"`
	// Layers are overlay lower directories, highest priority first.
	Layers []string    `json:"layers"`
	Spec   *specs.Spec `json:"spec"`
}

// Result of Compose.
type Result struct {
	Bundle *Bundle
	Cached bool
}

// Compose validates the lock and returns the bundle template, from cache
// when possible.
func Compose(l Layout, pl *manifest.PlatformLock, arch string) (*Result, error) {
	if pl == nil {
		return nil, fmt.Errorf("missing lock")
	}
	want := pl.ComputeDigest()
	if pl.BundleDigest != want {
		return nil, fmt.Errorf("lock digest mismatch: lock says %s, content is %s", pl.BundleDigest, want)
	}
	if !strings.HasSuffix(pl.Platform, "/"+arch) {
		return nil, fmt.Errorf("bundle is for %s but the guest is linux/%s", pl.Platform, arch)
	}
	cachePath := filepath.Join(l.BundleDir(want), "bundle.json")
	if b, err := os.ReadFile(cachePath); err == nil {
		var cached Bundle
		if json.Unmarshal(b, &cached) == nil && cached.Digest == want {
			if err := checkWarm(l, pl); err == nil {
				return &Result{Bundle: &cached, Cached: true}, nil
			}
		}
	}
	if err := checkWarm(l, pl); err != nil {
		return nil, err
	}
	if err := checkComponentConflicts(pl); err != nil {
		return nil, err
	}
	b := &Bundle{Digest: want, Layers: Layers(l, pl), Spec: Spec(l, pl)}
	data, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return nil, err
	}
	tmp := cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		return nil, err
	}
	return &Result{Bundle: b}, nil
}

// checkWarm makes sure every artifact the lock references is present in the
// guest cache. It only stats directories.
func checkWarm(l Layout, pl *manifest.PlatformLock) error {
	var missing []string
	if !isDir(l.BaseDir(pl.Base.Key)) {
		missing = append(missing, "base "+pl.Base.Ref)
	}
	for _, sp := range pl.StorePaths {
		if !exists(l.StorePath(sp.Name())) {
			missing = append(missing, sp.Path)
		}
	}
	for _, c := range pl.Components {
		if !isDir(l.ComponentDir(c.Key)) {
			missing = append(missing, "component "+c.Name)
		}
	}
	if len(missing) > 0 {
		if len(missing) > 5 {
			missing = append(missing[:5], fmt.Sprintf("... and %d more", len(missing)-5))
		}
		return fmt.Errorf("artifacts not warm in guest (run `oar warm`): %s", strings.Join(missing, ", "))
	}
	return nil
}

func checkComponentConflicts(pl *manifest.PlatformLock) error {
	owner := map[string]string{}
	for _, c := range pl.Components {
		for _, f := range c.Files {
			if strings.HasSuffix(f, "/") {
				continue
			}
			if prev, ok := owner[f]; ok {
				return fmt.Errorf("file conflict: /%s provided by %s and %s", f, prev, c.Name)
			}
			owner[f] = c.Name
		}
	}
	return nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Layers returns overlay lowerdirs: components (sorted by key, so the order
// is independent of configuration order; conflicts are rejected anyway)
// followed by the base rootfs.
func Layers(l Layout, pl *manifest.PlatformLock) []string {
	keys := make([]string, 0, len(pl.Components))
	for _, c := range pl.Components {
		keys = append(keys, c.Key)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, l.ComponentDir(k))
	}
	return append(out, l.BaseDir(pl.Base.Key))
}

var defaultCaps = []string{
	"CAP_AUDIT_WRITE", "CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID",
	"CAP_KILL", "CAP_MKNOD", "CAP_NET_BIND_SERVICE", "CAP_NET_RAW", "CAP_SETFCAP",
	"CAP_SETGID", "CAP_SETPCAP", "CAP_SETUID", "CAP_SYS_CHROOT",
}

// Spec generates the OCI runtime configuration. Instance specific fields
// (cgroupsPath) are filled in at instantiation.
func Spec(l Layout, pl *manifest.PlatformLock) *specs.Spec {
	caps := append([]string(nil), defaultCaps...)
	s := &specs.Spec{
		Version:  specs.Version,
		Hostname: pl.Hostname,
		Root:     &specs.Root{Path: "rootfs"},
		Process: &specs.Process{
			Args: pl.Process.Args,
			Env:  pl.Process.Env,
			Cwd:  pl.Process.Cwd,
			User: specs.User{UID: pl.Process.UID, GID: pl.Process.GID},
			Capabilities: &specs.LinuxCapabilities{
				Bounding: caps, Effective: caps, Permitted: caps,
			},
			Rlimits:         []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 65536, Soft: 65536}},
			NoNewPrivileges: true,
		},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777"}},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace}, {Type: specs.IPCNamespace}, {Type: specs.UTSNamespace},
				{Type: specs.MountNamespace}, {Type: specs.CgroupNamespace},
			},
			MaskedPaths: []string{
				"/proc/acpi", "/proc/asound", "/proc/kcore", "/proc/keys", "/proc/latency_stats",
				"/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug", "/sys/firmware", "/proc/scsi",
			},
			ReadonlyPaths: []string{
				"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
			},
		},
	}
	if len(pl.StorePaths) > 0 {
		s.Mounts = append(s.Mounts, specs.Mount{
			Destination: "/nix/store", Type: "bind", Source: l.StoreRoot(),
			Options: []string{"rbind", "ro", "nosuid", "nodev"},
		})
	}
	return s
}
