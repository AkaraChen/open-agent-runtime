package proto

import (
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
)

// InfoResult describes the guest.
type InfoResult struct {
	AgentVersion string `json:"agentVersion"`
	Protocol     int    `json:"protocol"`
	Arch         string `json:"arch"`
	Kernel       string `json:"kernel"`
	Root         string `json:"root"`
	Mode         string `json:"mode"` // vm or native
	Runc         string `json:"runc"`
	BootedAt     string `json:"bootedAt,omitempty"`
	Containers   int    `json:"containers"`
}

// Artifact kinds stored in the guest cache.
const (
	KindBase      = "base"
	KindStore     = "store"
	KindComponent = "component"
)

// WarmItem identifies one immutable artifact.
type WarmItem struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`            // sha256 of the artifact tarball
	Name string `json:"name,omitempty"` // store path basename for KindStore
	Size int64  `json:"size,omitempty"`
}

// WarmPutResult reports what one upload extracted.
type WarmPutResult struct {
	Files    int           `json:"files"`
	Bytes    int64         `json:"bytes"`
	Duration time.Duration `json:"duration"`
}

// StatsArgs tunes a stats request.
type StatsArgs struct {
	// DropCaches drops the guest page cache first (vm mode only), so cache
	// growth can be measured.
	DropCaches bool `json:"dropCaches,omitempty"`
}

// StatsResult is a snapshot of guest resource usage, for benchmarks and
// leak detection.
type StatsResult struct {
	CacheBytes   map[string]int64 `json:"cacheBytes"` // per cache area
	CacheFiles   map[string]int64 `json:"cacheFiles"`
	Mounts       int              `json:"mounts"`
	StoreViews   int              `json:"storeViews"`
	Instances    int              `json:"instances"`
	Cgroups      int              `json:"cgroups"`
	MemTotal     int64            `json:"memTotal"`
	MemAvailable int64            `json:"memAvailable"`
	Cached       int64            `json:"cached"`
	Uptime       float64          `json:"uptime"`
	Load1        float64          `json:"load1"`
	// Boot holds guest uptimes (seconds) at boot milestones (vm mode).
	Boot map[string]float64 `json:"boot,omitempty"`
}

// WarmStatusArgs asks which items are missing.
type WarmStatusArgs struct {
	Items []WarmItem `json:"items"`
}

// WarmStatusResult lists missing items.
type WarmStatusResult struct {
	Missing []WarmItem `json:"missing"`
}

// WarmPutArgs precedes a tarball body.
type WarmPutArgs struct {
	Item WarmItem `json:"item"`
}

// ComposeArgs composes a bundle from a platform lock.
type ComposeArgs struct {
	Lock *manifest.PlatformLock `json:"lock"`
}

// ComposeResult reports the composed bundle.
type ComposeResult struct {
	BundleDigest string        `json:"bundleDigest"`
	Cached       bool          `json:"cached"`
	Duration     time.Duration `json:"duration"`
	Layers       []string      `json:"layers"`
}

// Reload strategies.
const (
	// StrategyOverlap starts the new instance, then stops the old one. No
	// gap, but both run briefly (services need SO_REUSEPORT to share ports).
	StrategyOverlap = "overlap"
	// StrategyReplace creates the new instance (not yet exec'd), stops the
	// old one, then starts the new one. The gap is the old instance's exit
	// time plus `runc start`.
	StrategyReplace = "replace"
)

// RunArgs composes and starts a container.
type RunArgs struct {
	Name        string                 `json:"name"`
	Lock        *manifest.PlatformLock `json:"lock"`
	Strategy    string                 `json:"strategy,omitempty"`
	StopTimeout time.Duration          `json:"stopTimeout,omitempty"`
	// Start makes prepare also start the instance and wait until it is
	// ready (hot standby); it receives traffic only after the switch.
	Start bool `json:"start,omitempty"`
}

// Timings of a run, all measured inside the guest.
type Timings struct {
	Compose     time.Duration `json:"compose"`
	Instantiate time.Duration `json:"instantiate"`
	Create      time.Duration `json:"create"`
	Start       time.Duration `json:"start"`
	Ready       time.Duration `json:"ready,omitempty"`
	// Drain is how long the old instance kept serving in-flight connections
	// after the ingress switched to the new one.
	Drain   time.Duration `json:"drain,omitempty"`
	StopOld time.Duration `json:"stopOld,omitempty"`
	// Downtime is the window with no running instance (replace strategy).
	Downtime time.Duration `json:"downtime,omitempty"`
	Total    time.Duration `json:"total"`
}

// RunResult describes a started container.
type RunResult struct {
	Container Container `json:"container"`
	Cached    bool      `json:"cached"`
	Strategy  string    `json:"strategy,omitempty"`
	// Prepared is true when a pre-created instance was used.
	Prepared bool    `json:"prepared,omitempty"`
	Timings  Timings `json:"timings"`
	Replaced string  `json:"replaced,omitempty"`
}

// DialArgs opens a stream to a TCP port inside the guest network.
type DialArgs struct {
	Port int `json:"port"`
}

// Ingress is one public port served by the agent's ingress proxy.
type Ingress struct {
	Container string `json:"container"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Target    string `json:"target"`
	Instance  string `json:"instance"`
	Active    int64  `json:"active"`
}

// IngressList lists ingress ports.
type IngressList struct {
	Ingresses []Ingress `json:"ingresses"`
}

// NameArgs addresses a container by name.
type NameArgs struct {
	Name    string        `json:"name"`
	Timeout time.Duration `json:"timeout,omitempty"`
	Force   bool          `json:"force,omitempty"`
}

// LogsArgs requests container logs.
type LogsArgs struct {
	Name   string `json:"name"`
	Follow bool   `json:"follow,omitempty"`
}

// ExecArgs runs a command in a container.
type ExecArgs struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
	Env  []string `json:"env,omitempty"`
}

// ExecResult carries the outcome of an exec.
type ExecResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Container is the observable state of a managed container.
type Container struct {
	Name         string    `json:"name"`
	ID           string    `json:"id"`
	BundleDigest string    `json:"bundleDigest"`
	Status       string    `json:"status"` // created, running, stopped, unknown
	Pid          int       `json:"pid,omitempty"`
	Created      time.Time `json:"created"`
	Bundle       string    `json:"bundle"`
}

// ListResult lists containers.
type ListResult struct {
	Containers []Container `json:"containers"`
}
