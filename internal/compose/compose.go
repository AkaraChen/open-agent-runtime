// Package compose turns a bundle lock plus warm guest caches into an OCI
// bundle description. It is the hot path: it never evaluates Nix, touches
// the network, installs packages or copies file contents. Its only outputs
// are a small config.json and the ordered list of overlay layers.
package compose

import (
	"crypto/sha256"
	"encoding/hex"
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

// StoreView is the read-only /nix/store view of a closure (a hard-link tree
// of its store paths), bind-mounted into containers. Views are keyed by
// closure, so bundles that differ only outside /nix/store share one.
func (l Layout) StoreView(closure string) string { return filepath.Join(l.Root, "views", closure) }

// ClosureKey identifies a closure by its store path names.
func ClosureKey(pl *manifest.PlatformLock) string {
	names := make([]string, 0, len(pl.StorePaths))
	for _, sp := range pl.StorePaths {
		names = append(names, sp.Name())
	}
	sort.Strings(names)
	h := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return hex.EncodeToString(h[:16])
}

// StoreLayers holds one directory per store path.
func (l Layout) StoreLayers() string { return filepath.Join(l.Root, "store", "paths") }

// StoreLayer is the directory of one store path, whose only content is
// nix/store/<name>.
func (l Layout) StoreLayer(name string) string { return filepath.Join(l.StoreLayers(), name) }

// StorePath is a single extracted store path.
func (l Layout) StorePath(name string) string {
	return filepath.Join(l.StoreLayer(name), "nix", "store", name)
}

// ComponentDir is an extracted component tree.
func (l Layout) ComponentDir(key string) string {
	return filepath.Join(l.Root, "components", hexOf(key), "tree")
}

// BundleDir caches composed bundle templates by digest.
func (l Layout) BundleDir(digest string) string {
	return filepath.Join(l.Root, "bundles", hexOf(digest))
}

// CriuConfig is the CRIU configuration file used for this cache.
func (l Layout) CriuConfig() string { return filepath.Join(l.Root, "criu.conf") }

// SnapshotDir holds a bundle's checkpoint.
func (l Layout) SnapshotDir(digest string) string {
	return filepath.Join(l.BundleDir(digest), "snapshot")
}

// InstancesDir holds per-container instance directories.
func (l Layout) InstancesDir() string { return filepath.Join(l.Root, "instances") }

// Tmp is a scratch directory on the same filesystem as the caches.
func (l Layout) Tmp() string { return filepath.Join(l.Root, "tmp") }

// templateFormat is bumped whenever Spec or Layers change shape.
// 4: /nix/store is a per-bundle view of the closure, bound with one rbind.
// 5: private mount propagation (checkpoint/restore).
// 6: CRIU configuration annotation.
// 7: rootfs propagation moved to instantiation (depends on pivot_root).
// 8: store paths are overlay layers.
// 9: /nix/store is a read-only overlay view of the closure, bound once.
const templateFormat = 10

// Bundle is a cached, composed bundle template.
type Bundle struct {
	// Format invalidates cached templates when the generator changes.
	Format int    `json:"format"`
	Digest string `json:"digest"`
	// StoreLayers are the closure's store path directories (sorted by
	// name), linked by the agent into the read-only StoreView.
	StoreLayers []string `json:"storeLayers,omitempty"`
	StoreView   string   `json:"storeView,omitempty"`
	// Layers are the other overlay lower directories, highest priority
	// first: component trees, then the base rootfs (last).
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
		if json.Unmarshal(b, &cached) == nil && cached.Digest == want && cached.Format == templateFormat {
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
	b := &Bundle{Format: templateFormat, Digest: want, Layers: Layers(l, pl), StoreLayers: StoreLayers(l, pl), Spec: Spec(l, pl)}
	if len(pl.StorePaths) > 0 {
		b.StoreView = l.StoreView(ClosureKey(pl))
	}
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

// StoreLayers returns the closure's store path layers, sorted by name.
func StoreLayers(l Layout, pl *manifest.PlatformLock) []string {
	names := make([]string, 0, len(pl.StorePaths))
	for _, sp := range pl.StorePaths {
		names = append(names, sp.Name())
	}
	sort.Strings(names)
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = l.StoreLayer(n)
	}
	return out
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
		// runc hands this file to CRIU on checkpoint/restore instead of the
		// host-wide /etc/criu/runc.conf
		Annotations: map[string]string{"org.criu.config": l.CriuConfig()},
		Root:        &specs.Root{Path: "rootfs"},
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
		// The agent links the closure into one read-only view per closure;
		// each container gets it with a single bind.
		s.Mounts = append(s.Mounts, specs.Mount{
			Destination: "/nix/store", Type: "bind", Source: l.StoreView(ClosureKey(pl)),
			Options: []string{"bind", "ro", "nosuid", "nodev"},
		})
	}
	return s
}
