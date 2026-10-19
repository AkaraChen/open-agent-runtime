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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

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
	mu     sync.Mutex // serializes container lifecycle operations
	warmMu sync.Map   // per-key locks for warm.put
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
		return nil, s.warmPut(a.Item, body, req.BodySize)
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
		return proto.ComposeResult{BundleDigest: res.Bundle.Digest, Cached: res.Cached, Duration: time.Since(start), Layers: res.Bundle.Layers}, nil
	case proto.OpRun, proto.OpReload:
		var a proto.RunArgs
		if err := proto.Decode(req, &a); err != nil {
			return nil, err
		}
		return s.run(ctx, a, req.Op == proto.OpReload)
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
		Runc:         s.cfg.Runc.Version(),
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
func (s *Server) warmPut(it proto.WarmItem, body io.Reader, size int64) error {
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
	if err := extractTar(&cr, extractRoot); err != nil {
		return fmt.Errorf("extract %s %s: %w", it.Kind, it.Key, err)
	}
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
	dir          string
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
		if in.Name == name && !in.Retired {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	t0 := time.Now()
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

	res, err := compose.Compose(s.layout, a.Lock, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	tCompose := time.Now()

	in, err := s.instantiate(a.Name, res.Bundle)
	if err != nil {
		return nil, err
	}
	tInst := time.Now()

	logf, err := os.OpenFile(filepath.Join(in.dir, "console.log"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		s.destroy(ctx, in, true)
		return nil, err
	}
	defer logf.Close()
	if err := s.cfg.Runc.Create(ctx, in.ID, in.dir, logf); err != nil {
		s.destroy(ctx, in, true)
		return nil, err
	}
	tCreate := time.Now()
	if err := s.cfg.Runc.Start(ctx, in.ID); err != nil {
		s.destroy(ctx, in, true)
		return nil, err
	}
	tStart := time.Now()

	result := &proto.RunResult{
		Cached: res.Cached,
		Timings: proto.Timings{
			Compose:     tCompose.Sub(t0),
			Instantiate: tInst.Sub(tCompose),
			Create:      tCreate.Sub(tInst),
			Start:       tStart.Sub(tCreate),
		},
	}
	// reload: the new instance is running; retire and destroy the old one
	if old != nil {
		old.Retired = true
		_ = old.save()
		s.cfg.Runc.Kill(ctx, old.ID, "TERM")
		if !s.cfg.Runc.WaitStopped(ctx, old.ID, 10*time.Second) {
			s.cfg.Runc.Kill(ctx, old.ID, "KILL")
			s.cfg.Runc.WaitStopped(ctx, old.ID, 5*time.Second)
		}
		if err := s.destroy(ctx, old, true); err != nil {
			return nil, fmt.Errorf("new instance %s started, but removing %s failed: %w", in.ID, old.ID, err)
		}
		result.Replaced = old.ID
	}
	result.Timings.Total = time.Since(t0)
	result.Container = s.observe(ctx, in)
	return result, nil
}

// instantiate creates the per-instance bundle directory: an overlay mount of
// the cached layers plus a config.json derived from the cached template.
func (s *Server) instantiate(name string, b *compose.Bundle) (*instance, error) {
	var rnd [4]byte
	rand.Read(rnd[:])
	id := fmt.Sprintf("%s-%s", name, hex.EncodeToString(rnd[:]))
	in := &instance{Name: name, ID: id, BundleDigest: b.Digest, Created: time.Now().UTC(), dir: s.instDir(id)}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.current(a.Name)
	if err != nil {
		return nil, err
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.current(a.Name)
	if err != nil {
		return err
	}
	st := s.observe(ctx, in)
	if (st.Status == "running" || st.Status == "created") && !a.Force {
		return fmt.Errorf("container %q is %s; stop it first or use --force", a.Name, st.Status)
	}
	return s.destroy(ctx, in, true)
}

func (s *Server) list(ctx context.Context) (*proto.ListResult, error) {
	list, err := s.loadInstances()
	if err != nil {
		return nil, err
	}
	res := &proto.ListResult{Containers: []proto.Container{}}
	for _, in := range list {
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

// StopAll stops and removes every managed container.
func (s *Server) StopAll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, _ := s.loadInstances()
	for _, in := range list {
		s.cfg.Runc.Kill(ctx, in.ID, "TERM")
		if !s.cfg.Runc.WaitStopped(ctx, in.ID, 10*time.Second) {
			s.cfg.Runc.Kill(ctx, in.ID, "KILL")
			s.cfg.Runc.WaitStopped(ctx, in.ID, 5*time.Second)
		}
		s.destroy(ctx, in, true)
	}
}

// SetShutdown installs the shutdown hook.
func (s *Server) SetShutdown(fn func()) { s.cfg.Shutdown = fn }
