//go:build linux

// Package agent is the guest agent: it owns the guest artifact cache, composes
// OCI bundles and drives runc. The same code runs as PID 1's child inside the
// Linux guest VM and directly on Linux hosts (native backend).
package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// Version of the agent.
var Version = "0.1.0"

// Config configures an agent.
type Config struct {
	Root     string // cache + state directory, e.g. /var/lib/oar
	Runc     Runc
	Mode     string // "vm" or "native"
	Shutdown func() // invoked by the shutdown op (vm mode)
}

// Server implements the agent protocol.
type Server struct {
	cfg    Config
	layout compose.Layout
	booted time.Time
	// mu is held shared by per-container operations and exclusively by
	// operations on all containers (StopAll); names serializes operations
	// on the same container name, so different containers start in parallel.
	mu    sync.RWMutex
	names sync.Map // name -> *sync.Mutex
	// cleanups tracks background removal of retired instances
	cleanups sync.WaitGroup
	views    storeViews
	ing      ingresses

	runcVerOnce sync.Once
	runcVer     string
	warmMu      sync.Map // per-key locks for warm.put
}

// New prepares the state directories.
func New(cfg Config) (*Server, error) {
	if cfg.Runc.Binary == "" {
		cfg.Runc.Binary = "runc"
	}
	if cfg.Runc.Root == "" {
		cfg.Runc.Root = "/run/oar/runc"
	}
	s := &Server{cfg: cfg, layout: compose.Layout{Root: cfg.Root}, booted: time.Now()}
	for _, d := range []string{cfg.Root, s.layout.Tmp(), s.layout.InstancesDir(), s.layout.StoreRoot(), cfg.Runc.Root,
		filepath.Join(cfg.Root, "bases"), filepath.Join(cfg.Root, "components"), filepath.Join(cfg.Root, "bundles")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// leftovers from interrupted transfers
	entries, _ := os.ReadDir(s.layout.Tmp())
	for _, e := range entries {
		os.RemoveAll(filepath.Join(s.layout.Tmp(), e.Name()))
	}
	return s, nil
}

// runcVersion is computed once: exec'ing runc on every info call costs
// tens of milliseconds in slow guests, and every CLI command calls info.
func (s *Server) runcVersion() string {
	s.runcVerOnce.Do(func() { s.runcVer = s.cfg.Runc.Version() })
	return s.runcVer
}

// Handle dispatches a request and logs failures to the agent's stderr
// (the guest console).
func (s *Server) Handle(ctx context.Context, req *proto.Request, body io.Reader, rs *proto.ResponseStream) (any, error) {
	res, err := s.handle(ctx, req, body, rs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "oar-agent: %s failed: %v\n", req.Op, err)
	}
	return res, err
}

func (s *Server) handle(ctx context.Context, req *proto.Request, body io.Reader, rs *proto.ResponseStream) (any, error) {
	switch req.Op {
	case proto.OpInfo:
		return s.info(), nil
	case proto.OpWarmStatus:
		var a proto.WarmStatusArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return s.warmStatus(a), nil
	case proto.OpWarmPut:
		var a proto.WarmPutArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return s.warmPut(a.Item, body, req.BodySize)
	case proto.OpCompose:
		var a proto.ComposeArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		start := time.Now()
		res, err := compose.Compose(s.layout, a.Lock, runtime.GOARCH)
		if err != nil {
			return nil, err
		}
		if err := s.ensureStoreView(res.Bundle); err != nil {
			return nil, err
		}
		return proto.ComposeResult{BundleDigest: res.Bundle.Digest, Cached: res.Cached, Duration: time.Since(start), Layers: res.Bundle.Layers}, nil
	case proto.OpRun, proto.OpReload:
		var a proto.RunArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return s.run(ctx, a, req.Op == proto.OpReload)
	case proto.OpDial:
		var a proto.DialArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return nil, s.handleDial(a, rs)
	case proto.OpIngress:
		return s.ingressList(), nil
	case proto.OpPrepare:
		var a proto.RunArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return s.prepare(ctx, a)
	case proto.OpStop:
		var a proto.NameArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return s.stop(ctx, a)
	case proto.OpDelete:
		var a proto.NameArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return nil, s.delete(ctx, a)
	case proto.OpList:
		return s.list(ctx)
	case proto.OpState:
		var a proto.NameArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		inst, err := s.current(a.Name)
		if err != nil {
			return nil, err
		}
		return s.observe(ctx, inst), nil
	case proto.OpLogs:
		var a proto.LogsArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return nil, s.logs(ctx, a, rs)
	case proto.OpExec:
		var a proto.ExecArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		inst, err := s.current(a.Name)
		if err != nil {
			return nil, err
		}
		code, out, errOut, err := s.cfg.Runc.Exec(ctx, inst.ID, a.Env, a.Args)
		if err != nil {
			return nil, err
		}
		return proto.ExecResult{ExitCode: code, Stdout: out, Stderr: errOut}, nil
	case proto.OpStats:
		var a proto.StatsArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		if a.DropCaches {
			if s.cfg.Mode != "vm" {
				return nil, fmt.Errorf("dropping caches is only allowed inside the guest VM")
			}
			unix.Sync()
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
				return nil, err
			}
		}
		return s.stats(), nil
	case proto.OpShutdown:
		if s.cfg.Shutdown == nil {
			return nil, fmt.Errorf("shutdown is only supported inside the guest VM")
		}
		go func() { time.Sleep(100 * time.Millisecond); s.cfg.Shutdown() }()
		return map[string]string{"status": "shutting down"}, nil
	}
	return nil, fmt.Errorf("unknown op %q", req.Op)
}

func (s *Server) info() proto.InfoResult {
	var u unix.Utsname
	unix.Uname(&u)
	list, _ := os.ReadDir(s.layout.InstancesDir())
	return proto.InfoResult{
		AgentVersion: Version,
		Protocol:     proto.Version,
		Arch:         runtime.GOARCH,
		Kernel:       unix.ByteSliceToString(u.Release[:]),
		Root:         s.cfg.Root,
		Mode:         s.cfg.Mode,
		Runc:         s.runcVersion(),
		BootedAt:     s.booted.Format(time.RFC3339),
		Containers:   len(list),
	}
}

func (s *Server) itemPath(it proto.WarmItem) (string, error) {
	switch it.Kind {
	case proto.KindBase:
		return filepath.Dir(s.layout.BaseDir(it.Key)), nil
	case proto.KindComponent:
		return filepath.Dir(s.layout.ComponentDir(it.Key)), nil
	case proto.KindStore:
		if it.Name == "" || strings.ContainsAny(it.Name, "/\x00") || strings.HasPrefix(it.Name, ".") {
			return "", fmt.Errorf("invalid store path name %q", it.Name)
		}
		return s.layout.StorePath(it.Name), nil
	}
	return "", fmt.Errorf("unknown artifact kind %q", it.Kind)
}

func (s *Server) warmStatus(a proto.WarmStatusArgs) proto.WarmStatusResult {
	res := proto.WarmStatusResult{Missing: []proto.WarmItem{}}
	for _, it := range a.Items {
		p, err := s.itemPath(it)
		if err != nil {
			res.Missing = append(res.Missing, it)
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			res.Missing = append(res.Missing, it)
		}
	}
	return res
}

// warmPut receives a tarball, verifies its sha256 against the key while
// extracting it into a staging directory, then atomically publishes it.
func (s *Server) warmPut(it proto.WarmItem, body io.Reader, size int64) (*proto.WarmPutResult, error) {
	start := time.Now()
	res := &proto.WarmPutResult{}
	err := s.warmPutInto(it, body, size, res)
	res.Duration = time.Since(start)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Server) warmPutInto(it proto.WarmItem, body io.Reader, size int64, res *proto.WarmPutResult) error {
	final, err := s.itemPath(it)
	if err != nil {
		return err
	}
	keyHex := it.Key
	if _, h, ok := strings.Cut(it.Key, ":"); ok {
		keyHex = h
	}
	muAny, _ := s.warmMu.LoadOrStore(final, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Lstat(final); err == nil {
		_, _ = io.Copy(io.Discard, body)
		return nil
	}
	stage, err := os.MkdirTemp(s.layout.Tmp(), "warm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	h := sha256.New()
	var cr countReader
	cr.r = io.TeeReader(body, h)
	extractRoot := stage
	if it.Kind != proto.KindStore {
		extractRoot = filepath.Join(stage, "x", leafName(it.Kind))
		if err := os.MkdirAll(extractRoot, 0o755); err != nil {
			return err
		}
	}
	files, err := extractTar(&cr, extractRoot)
	if err != nil {
		return fmt.Errorf("extract %s %s: %w", it.Kind, it.Key, err)
	}
	res.Files, res.Bytes = files, size
	if _, err := io.Copy(io.Discard, &cr); err != nil {
		return err
	}
	if size > 0 && cr.n != size {
		return fmt.Errorf("short body for %s: got %d of %d bytes", it.Key, cr.n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != keyHex {
		return fmt.Errorf("checksum mismatch for %s %s: got sha256:%s", it.Kind, it.Name, got)
	}
	src := filepath.Join(stage, "x")
	if it.Kind == proto.KindStore {
		src = filepath.Join(stage, "nix", "store", it.Name)
	}
	if _, err := os.Lstat(src); err != nil {
		return fmt.Errorf("artifact %s does not contain the expected tree", it.Key)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	return os.Rename(src, final)
}

func leafName(kind string) string {
	if kind == proto.KindBase {
		return "rootfs"
	}
	return "tree"
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ---- containers ----

type instance struct {
	Name         string    `json:"name"`
	ID           string    `json:"id"`
	BundleDigest string    `json:"bundleDigest"`
	Created      time.Time `json:"created"`
	Retired      bool      `json:"retired,omitempty"`
	// Prepared instances are created (not started) ahead of a switch;
	// Started marks a prepared instance that already runs (hot standby).
	Prepared bool `json:"prepared,omitempty"`
	Started  bool `json:"started,omitempty"`
	// Ports maps ingress port names to this instance's private ports.
	Ports       map[string]int  `json:"ports,omitempty"`
	PublicPorts []manifest.Port `json:"publicPorts,omitempty"`
	dir         string
}

func (s *Server) instDir(id string) string { return filepath.Join(s.layout.InstancesDir(), id) }

func (s *Server) loadInstances() ([]*instance, error) {
	entries, err := os.ReadDir(s.layout.InstancesDir())
	if err != nil {
		return nil, err
	}
	var out []*instance
	for _, e := range entries {
		var in instance
		b, err := os.ReadFile(filepath.Join(s.instDir(e.Name()), "instance.json"))
		if err != nil || json.Unmarshal(b, &in) != nil {
			continue
		}
		in.dir = s.instDir(e.Name())
		out = append(out, &in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// current returns the active instance for a name.
func (s *Server) current(name string) (*instance, error) {
	list, err := s.loadInstances()
	if err != nil {
		return nil, err
	}
	var found *instance
	for _, in := range list {
		if in.Name == name && !in.Retired && !in.Prepared {
			found = in
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no container named %q", name)
	}
	return found, nil
}

func (in *instance) save() error {
	b, _ := json.MarshalIndent(in, "", "  ")
	tmp := filepath.Join(in.dir, "instance.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(in.dir, "instance.json"))
}

func validName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func (s *Server) run(ctx context.Context, a proto.RunArgs, reload bool) (*proto.RunResult, error) {
	if !validName(a.Name) {
		return nil, fmt.Errorf("invalid container name %q", a.Name)
	}
	switch a.Strategy {
	case "":
		a.Strategy = proto.StrategyOverlap
	case proto.StrategyOverlap, proto.StrategyReplace:
	default:
		return nil, fmt.Errorf("unknown reload strategy %q (want overlap or replace)", a.Strategy)
	}
	t0 := time.Now()
	unlock := s.lockName(a.Name)
	defer unlock()
	var err error
	old, _ := s.current(a.Name)
	if old != nil && !reload {
		st := s.observe(ctx, old)
		if st.Status == "running" || st.Status == "created" {
			return nil, fmt.Errorf("container %q is already %s (use reload)", a.Name, st.Status)
		}
		// replace a stopped instance with the same name
		if err := s.destroy(ctx, old, true); err != nil {
			return nil, err
		}
		old = nil
	}

	var in *instance
	var cached, prepared bool
	var tCompose, tInst time.Time
	if p := s.takePrepared(ctx, a.Name, a.Lock.BundleDigest); p != nil {
		in, cached, prepared = p, true, true
		tCompose = time.Now()
		tInst = tCompose
	} else {
		in, cached, tCompose, tInst, err = s.createInstance(ctx, a.Name, a)
		if err != nil {
			return nil, err
		}
	}
	tCreate := time.Now()
	result := &proto.RunResult{
		Cached:   cached,
		Prepared: prepared,
		Strategy: a.Strategy,
		Timings: proto.Timings{
			Compose:     tCompose.Sub(t0),
			Instantiate: tInst.Sub(tCompose),
			Create:      tCreate.Sub(tInst),
		},
	}
	grace := a.StopTimeout
	if grace <= 0 {
		grace = 10 * time.Second
	}
	stopOld := func() {
		old.Retired = true
		_ = old.save()
		s.cfg.Runc.Kill(ctx, old.ID, "TERM")
		if !s.cfg.Runc.WaitStopped(ctx, old.ID, grace) {
			s.cfg.Runc.Kill(ctx, old.ID, "KILL")
			s.cfg.Runc.WaitStopped(ctx, old.ID, 5*time.Second)
		}
	}
	standby := prepared && in.Started
	ready := func() error {
		if standby {
			return nil // a hot standby is already running and ready
		}
		tR := time.Now()
		err := s.waitReady(ctx, in, readyCheck(a.Lock, in))
		result.Timings.Ready = time.Since(tR)
		return err
	}
	startNew := func() error {
		if standby {
			return nil
		}
		return s.cfg.Runc.Start(ctx, in.ID)
	}
	// switchTraffic repoints the ingress at the new instance and waits for
	// the old instance's in-flight connections.
	switchTraffic := func() error {
		if len(in.PublicPorts) == 0 {
			return nil
		}
		olds, err := s.route(in)
		if err != nil {
			return err
		}
		if old != nil && len(olds) > 0 {
			result.Timings.Drain = drain(ctx, olds, grace)
		}
		return nil
	}
	if old != nil && a.Strategy == proto.StrategyReplace {
		// The new container is fully created (namespaces, mounts, cgroup)
		// but its process has not exec'd yet, so it holds no ports. Stop
		// the old one, then start the new one immediately.
		tStop := time.Now()
		stopOld()
		result.Timings.StopOld = time.Since(tStop)
		tS := time.Now()
		if err := startNew(); err != nil {
			s.destroy(ctx, in, true)
			old.Retired = false
			_ = old.save()
			return nil, fmt.Errorf("start new instance (old %s already stopped): %w", old.ID, err)
		}
		result.Timings.Start = time.Since(tS)
		if err := ready(); err != nil {
			return nil, fmt.Errorf("new instance %s started but is not ready (old %s already stopped): %w", in.ID, old.ID, err)
		}
		if err := switchTraffic(); err != nil {
			return nil, err
		}
		result.Timings.Downtime = time.Since(tStop)
	} else {
		tS := time.Now()
		if err := startNew(); err != nil {
			s.destroy(ctx, in, true)
			return nil, err
		}
		result.Timings.Start = time.Since(tS)
		if err := ready(); err != nil {
			if old != nil {
				// atomic switch: keep serving from the old instance
				s.cfg.Runc.Kill(ctx, in.ID, "KILL")
				s.cfg.Runc.WaitStopped(ctx, in.ID, 5*time.Second)
				s.destroy(ctx, in, true)
				return nil, fmt.Errorf("new instance not ready, rolled back to %s: %w", old.ID, err)
			}
			return nil, fmt.Errorf("container %s started but is not ready: %w", in.ID, err)
		}
		if err := switchTraffic(); err != nil {
			if old != nil {
				s.cfg.Runc.Kill(ctx, in.ID, "KILL")
				s.cfg.Runc.WaitStopped(ctx, in.ID, 5*time.Second)
				s.destroy(ctx, in, true)
				s.route(old) // keep the old instance serving
			}
			return nil, err
		}
		if old != nil {
			// overlap: the new instance is serving; retire the old one
			tStop := time.Now()
			stopOld()
			result.Timings.StopOld = time.Since(tStop)
		}
	}
	if old != nil {
		result.Replaced = old.ID
		// cleanup is off the critical path; the retired instance is
		// already invisible to name lookups
		// it does not take s.mu, so it never delays the next switch
		s.cleanups.Add(1)
		go func(old *instance) {
			defer s.cleanups.Done()
			if err := s.destroy(context.Background(), old, true); err != nil {
				fmt.Fprintf(os.Stderr, "oar-agent: cleanup of %s failed: %v\n", old.ID, err)
			}
		}(old)
	}
	result.Timings.Total = time.Since(t0)
	result.Container = s.observe(ctx, in)
	return result, nil
}

// createInstance composes the bundle, instantiates it and runs
// `runc create`. The container's process is prepared but not started.
func (s *Server) createInstance(ctx context.Context, name string, a proto.RunArgs) (*instance, bool, time.Time, time.Time, error) {
	res, err := compose.Compose(s.layout, a.Lock, runtime.GOARCH)
	if err != nil {
		return nil, false, time.Time{}, time.Time{}, err
	}
	if err := s.ensureStoreView(res.Bundle); err != nil {
		return nil, false, time.Time{}, time.Time{}, err
	}
	tCompose := time.Now()
	ports, err := allocatePorts(a.Lock.Ports)
	if err != nil {
		return nil, false, tCompose, time.Time{}, err
	}
	var env []string
	for _, p := range a.Lock.Ports {
		env = append(env, fmt.Sprintf("%s=%d", p.Env, ports[p.Name]))
	}
	in, err := s.instantiate(name, res.Bundle, env, func(in *instance) {
		in.Ports, in.PublicPorts = ports, a.Lock.Ports
	})
	if err != nil {
		return nil, false, tCompose, time.Time{}, err
	}
	tInst := time.Now()
	logf, err := os.OpenFile(filepath.Join(in.dir, "console.log"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		s.destroy(ctx, in, true)
		return nil, false, tCompose, tInst, err
	}
	defer logf.Close()
	if err := s.cfg.Runc.Create(ctx, in.ID, in.dir, logf); err != nil {
		s.destroy(ctx, in, true)
		return nil, false, tCompose, tInst, err
	}
	return in, res.Cached, tCompose, tInst, nil
}

// takePrepared returns the prepared instance for name if it matches digest
// (and discards a prepared instance for another bundle).
func (s *Server) takePrepared(ctx context.Context, name, digest string) *instance {
	list, err := s.loadInstances()
	if err != nil {
		return nil
	}
	var found *instance
	for _, in := range list {
		if in.Name != name || !in.Prepared {
			continue
		}
		if in.BundleDigest == digest && found == nil {
			if st, err := s.cfg.Runc.State(ctx, in.ID); err == nil && (st.Status == "created" || in.Started && st.Status == "running") {
				found = in
				continue
			}
		}
		s.destroy(ctx, in, true)
	}
	if found != nil {
		found.Prepared = false
		found.Created = time.Now().UTC()
		if err := found.save(); err != nil {
			return nil
		}
	}
	return found
}

// prepare creates (but does not start) an instance of a bundle for name,
// so the next run/reload of that bundle skips compose, mount and create.
func (s *Server) prepare(ctx context.Context, a proto.RunArgs) (*proto.RunResult, error) {
	if !validName(a.Name) {
		return nil, fmt.Errorf("invalid container name %q", a.Name)
	}
	t0 := time.Now()
	unlock := s.lockName(a.Name)
	defer unlock()
	// one prepared instance per name
	if list, err := s.loadInstances(); err == nil {
		for _, in := range list {
			if in.Name == a.Name && in.Prepared {
				if in.BundleDigest == a.Lock.BundleDigest {
					return &proto.RunResult{Container: s.observe(ctx, in), Cached: true}, nil
				}
				s.destroy(ctx, in, true)
			}
		}
	}
	in, cached, tCompose, tInst, err := s.createInstance(ctx, a.Name, a)
	if err != nil {
		return nil, err
	}
	in.Prepared = true
	tCreate := time.Now()
	var tStart, tReady time.Duration
	if a.Start {
		// hot standby: running and ready, but the ingress does not route
		// to it until the switch
		t := time.Now()
		if err := s.cfg.Runc.Start(ctx, in.ID); err != nil {
			s.destroy(ctx, in, true)
			return nil, err
		}
		tStart = time.Since(t)
		t = time.Now()
		if err := s.waitReady(ctx, in, readyCheck(a.Lock, in)); err != nil {
			s.cfg.Runc.Kill(ctx, in.ID, "KILL")
			s.cfg.Runc.WaitStopped(ctx, in.ID, 5*time.Second)
			s.destroy(ctx, in, true)
			return nil, fmt.Errorf("standby not ready: %w", err)
		}
		tReady = time.Since(t)
		in.Started = true
	}
	if err := in.save(); err != nil {
		s.destroy(ctx, in, true)
		return nil, err
	}
	return &proto.RunResult{
		Container: s.observe(ctx, in),
		Cached:    cached,
		Timings: proto.Timings{
			Compose: tCompose.Sub(t0), Instantiate: tInst.Sub(tCompose),
			Create: tCreate.Sub(tInst), Start: tStart, Ready: tReady, Total: time.Since(t0),
		},
	}, nil
}

// readyCheck returns the readiness check for an instance: the configured
// one, or a TCP check on the first private ingress port.
func readyCheck(pl *manifest.PlatformLock, in *instance) *manifest.Ready {
	if pl.Ready != nil {
		return pl.Ready
	}
	if len(pl.Ports) > 0 {
		return &manifest.Ready{TCP: fmt.Sprintf("127.0.0.1:%d", in.Ports[pl.Ports[0].Name])}
	}
	return nil
}

// waitReady polls a readiness check until it succeeds or times out.
func (s *Server) waitReady(ctx context.Context, in *instance, r *manifest.Ready) error {
	if r == nil {
		return nil
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		switch {
		case r.TCP != "":
			c, err := net.DialTimeout("tcp", r.TCP, 200*time.Millisecond)
			if err == nil {
				c.Close()
				return nil
			}
			last = err
		case len(r.Exec) > 0:
			code, _, stderr, err := s.cfg.Runc.Exec(ctx, in.ID, nil, r.Exec)
			if err == nil && code == 0 {
				return nil
			}
			last = fmt.Errorf("exit %d: %v %s", code, err, strings.TrimSpace(stderr))
		case r.Log != "":
			b, err := os.ReadFile(filepath.Join(in.dir, "console.log"))
			if err == nil && strings.Contains(string(b), r.Log) {
				return nil
			}
			last = fmt.Errorf("%q not in output yet", r.Log)
		}
		if st := s.observe(ctx, in); st.Status == "stopped" || st.Status == "deleted" {
			return fmt.Errorf("container exited before becoming ready")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("readiness timed out after %s: %v", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// instantiate creates the per-instance bundle directory: an overlay mount of
// the cached layers plus a config.json derived from the cached template.
func (s *Server) instantiate(name string, b *compose.Bundle, extraEnv []string, init func(*instance)) (*instance, error) {
	var rnd [4]byte
	rand.Read(rnd[:])
	id := fmt.Sprintf("%s-%s", name, hex.EncodeToString(rnd[:]))
	in := &instance{Name: name, ID: id, BundleDigest: b.Digest, Created: time.Now().UTC(), dir: s.instDir(id)}
	if init != nil {
		init(in)
	}
	for _, d := range []string{"rootfs", "upper", "work"} {
		if err := os.MkdirAll(filepath.Join(in.dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(b.Layers, ":"), filepath.Join(in.dir, "upper"), filepath.Join(in.dir, "work"))
	if err := unix.Mount("overlay", filepath.Join(in.dir, "rootfs"), "overlay", 0, opts); err != nil {
		os.RemoveAll(in.dir)
		return nil, fmt.Errorf("mount overlay: %w", err)
	}
	spec := *b.Spec
	if len(extraEnv) > 0 {
		proc := *spec.Process
		proc.Env = append(append([]string(nil), proc.Env...), extraEnv...)
		spec.Process = &proc
	}
	linux := *spec.Linux
	linux.CgroupsPath = "/oar/" + id
	spec.Linux = &linux
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		spec.Mounts = append(append([]specMount(nil), spec.Mounts...), specMount{
			Destination: "/etc/resolv.conf", Type: "bind", Source: "/etc/resolv.conf",
			Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"},
		})
	}
	data, err := json.MarshalIndent(&spec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(in.dir, "config.json"), data, 0o644); err != nil {
		return nil, err
	}
	if err := in.save(); err != nil {
		return nil, err
	}
	return in, nil
}

// destroy removes a container and its instance directory.
func (s *Server) destroy(ctx context.Context, in *instance, force bool) error {
	if err := s.cfg.Runc.Delete(ctx, in.ID, force); err != nil {
		return err
	}
	rootfs := filepath.Join(in.dir, "rootfs")
	if err := unix.Unmount(rootfs, unix.MNT_DETACH); err != nil && err != unix.EINVAL && err != unix.ENOENT {
		return fmt.Errorf("unmount %s: %w", rootfs, err)
	}
	return os.RemoveAll(in.dir)
}

func (s *Server) observe(ctx context.Context, in *instance) proto.Container {
	c := proto.Container{Name: in.Name, ID: in.ID, BundleDigest: in.BundleDigest, Created: in.Created, Bundle: in.dir, Status: "unknown"}
	if st, err := s.cfg.Runc.State(ctx, in.ID); err == nil {
		c.Status, c.Pid = st.Status, st.Pid
		if st.Status == "stopped" {
			c.Pid = 0
		}
	}
	return c
}

func (s *Server) stop(ctx context.Context, a proto.NameArgs) (*proto.Container, error) {
	unlock := s.lockName(a.Name)
	defer unlock()
	in, err := s.current(a.Name)
	if err != nil {
		return nil, err
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	s.unroute(a.Name, in.ID)
	st := s.observe(ctx, in)
	if st.Status == "running" || st.Status == "created" || st.Status == "paused" {
		if err := s.cfg.Runc.Kill(ctx, in.ID, "TERM"); err != nil && !strings.Contains(err.Error(), "not running") {
			return nil, err
		}
		if !s.cfg.Runc.WaitStopped(ctx, in.ID, timeout) {
			s.cfg.Runc.Kill(ctx, in.ID, "KILL")
			s.cfg.Runc.WaitStopped(ctx, in.ID, 5*time.Second)
		}
	}
	c := s.observe(ctx, in)
	return &c, nil
}

func (s *Server) delete(ctx context.Context, a proto.NameArgs) error {
	unlock := s.lockName(a.Name)
	defer unlock()
	in, err := s.current(a.Name)
	if err != nil {
		return err
	}
	st := s.observe(ctx, in)
	if (st.Status == "running" || st.Status == "created") && !a.Force {
		return fmt.Errorf("container %q is %s; stop it first or use --force", a.Name, st.Status)
	}
	s.unroute(a.Name, in.ID)
	if err := s.destroy(ctx, in, true); err != nil {
		return err
	}
	// a prepared instance for a deleted name would never be used
	if list, err := s.loadInstances(); err == nil {
		for _, p := range list {
			if p.Name == a.Name && p.Prepared {
				s.destroy(ctx, p, true)
			}
		}
	}
	return nil
}

func (s *Server) list(ctx context.Context) (*proto.ListResult, error) {
	list, err := s.loadInstances()
	if err != nil {
		return nil, err
	}
	res := &proto.ListResult{Containers: []proto.Container{}}
	for _, in := range list {
		if in.Retired || in.Prepared {
			continue // being cleaned up after a reload, or not started yet
		}
		res.Containers = append(res.Containers, s.observe(ctx, in))
	}
	return res, nil
}

func (s *Server) logs(ctx context.Context, a proto.LogsArgs, rs *proto.ResponseStream) error {
	in, err := s.current(a.Name)
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(in.dir, "console.log"))
	if err != nil {
		return err
	}
	defer f.Close()
	if !a.Follow {
		st, err := f.Stat()
		if err != nil {
			return err
		}
		w, err := rs.Begin(nil, st.Size())
		if err != nil {
			return err
		}
		_, err = io.CopyN(w, f, st.Size())
		return err
	}
	w, err := rs.Begin(nil, -1)
	if err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil
			}
			continue
		}
		if err != nil && err != io.EOF {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
		if st := s.observe(ctx, in); st.Status == "stopped" || st.Status == "deleted" {
			// drain whatever is left
			io.Copy(w, f)
			return nil
		}
	}
}

// lockName takes the shared lifecycle lock plus the lock for one name.
func (s *Server) lockName(name string) func() {
	s.mu.RLock()
	m, _ := s.names.LoadOrStore(name, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return func() {
		mu.Unlock()
		s.mu.RUnlock()
	}
}

// StopAll stops and removes every managed container.
func (s *Server) StopAll(ctx context.Context) {
	s.cleanups.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeAllIngress()
	list, _ := s.loadInstances()
	for _, in := range list {
		s.cfg.Runc.Kill(ctx, in.ID, "TERM")
		if !s.cfg.Runc.WaitStopped(ctx, in.ID, 10*time.Second) {
			s.cfg.Runc.Kill(ctx, in.ID, "KILL")
			s.cfg.Runc.WaitStopped(ctx, in.ID, 5*time.Second)
		}
		s.destroy(ctx, in, true)
	}
	s.cleanups.Wait()
	s.unmountAllStoreViews()
}

// SetShutdown installs the shutdown hook.
func (s *Server) SetShutdown(fn func()) { s.cfg.Shutdown = fn }
