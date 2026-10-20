// Package vm defines the VM backend abstraction and the host-side supervisor
// that keeps a resident Linux guest running and exposes its agent on a local
// unix socket.
package vm

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// MachineConfig configures a guest VM.
type MachineConfig struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"`  // per-VM state directory
	Arch      string `json:"arch"` // amd64 or arm64
	CPUs      int    `json:"cpus"`
	MemoryMiB int    `json:"memoryMiB"`
	DiskGiB   int    `json:"diskGiB"`
	Kernel    string `json:"kernel,omitempty"`
	Initrd    string `json:"initrd,omitempty"`
	RootFSTar string `json:"rootfsTar,omitempty"` // WSL2 import source
	// Root is the cache directory for the native backend.
	Root string `json:"root,omitempty"`
	// ConsoleLog receives the guest serial console.
	ConsoleLog string `json:"consoleLog"`
}

// Machine is a running guest.
type Machine interface {
	// DialAgent opens a new stream to the guest agent.
	DialAgent(ctx context.Context) (net.Conn, error)
	// Done is closed when the guest has exited.
	Done() <-chan struct{}
	// Kill forcefully stops the guest.
	Kill() error
}

// Backend boots machines.
type Backend interface {
	Name() string
	// Available reports why the backend cannot be used on this host.
	Available() error
	// NeedsGuestImage is false for backends that run the agent on the host.
	NeedsGuestImage() bool
	// NeedsRoot reports whether the supervisor must run as root.
	NeedsRoot() bool
	Boot(ctx context.Context, cfg MachineConfig) (Machine, error)
}

var (
	regMu    sync.Mutex
	registry = map[string]Backend{}
)

// Register makes a backend available by name.
func Register(b Backend) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[b.Name()] = b
}

// Get returns a backend by name.
func Get(name string) (Backend, error) {
	regMu.Lock()
	defer regMu.Unlock()
	b, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (available: %s)", name, strings.Join(namesLocked(), ", "))
	}
	return b, nil
}

// Names lists registered backends.
func Names() []string {
	regMu.Lock()
	defer regMu.Unlock()
	return namesLocked()
}

func namesLocked() []string {
	var n []string
	for k := range registry {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// DefaultBackend is the preferred backend for this host OS.
func DefaultBackend() string {
	if b := os.Getenv("OAR_BACKEND"); b != "" {
		return b
	}
	switch runtime.GOOS {
	case "darwin":
		// prefer Virtualization.framework, fall back to QEMU (HVF or TCG)
		if b, err := Get("vz"); err == nil && b.Available() == nil {
			return "vz"
		}
		if b, err := Get("qemu"); err == nil && b.Available() == nil {
			return "qemu"
		}
		return "vz"
	case "windows":
		return "wsl2"
	}
	return "native"
}

// Home is the oar state directory.
func Home() string {
	if h := os.Getenv("OAR_HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "oar")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "oar")
}

// Dir is the state directory of a named VM.
func Dir(home, name string) string { return filepath.Join(home, "vms", name) }

// SocketPath is the host-side agent socket of a VM.
func SocketPath(dir string) string { return filepath.Join(dir, "agent.sock") }

// State is persisted in <dir>/state.json while a supervisor runs.
type State struct {
	Name    string        `json:"name"`
	Backend string        `json:"backend"`
	Pid     int           `json:"pid"`
	Socket  string        `json:"socket"`
	Started time.Time     `json:"started"`
	Ready   bool          `json:"ready"`
	Boot    time.Duration `json:"bootDuration"`
	// Phases are offsets from Started: machine booted by the backend, agent
	// first reachable, host socket ready.
	Phases map[string]time.Duration `json:"phases,omitempty"`
	Config MachineConfig            `json:"config"`
	Error  string                   `json:"error,omitempty"`
}

// ReadState loads the state of a VM.
func ReadState(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeState(dir string, s *State, owner *Owner) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	p := filepath.Join(dir, "state.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	owner.chown(tmp)
	return os.Rename(tmp, p)
}

// Owner is the unprivileged user that should own files created by a
// supervisor running as root (native backend under sudo).
type Owner struct{ UID, GID int }

func (o *Owner) chown(p string) {
	if o != nil {
		os.Lchown(p, o.UID, o.GID)
	}
}

// Client returns a protocol client for the VM's host socket.
func Client(dir string) *proto.Client {
	sock := SocketPath(dir)
	return &proto.Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
}

// Supervise boots the machine and serves its agent on the host socket until
// ctx is cancelled, then shuts the guest down.
func Supervise(ctx context.Context, b Backend, cfg MachineConfig, owner *Owner, log io.Writer) error {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return err
	}
	owner.chown(cfg.Dir)
	st := &State{Name: cfg.Name, Backend: b.Name(), Pid: os.Getpid(), Socket: SocketPath(cfg.Dir), Started: time.Now(), Config: cfg}
	if err := writeState(cfg.Dir, st, owner); err != nil {
		return err
	}
	fail := func(err error) error {
		st.Error = err.Error()
		writeState(cfg.Dir, st, owner)
		return err
	}
	fmt.Fprintf(log, "booting %s guest %q (%s, %d cpus, %d MiB)\n", b.Name(), cfg.Name, cfg.Arch, cfg.CPUs, cfg.MemoryMiB)
	st.Phases = map[string]time.Duration{}
	m, err := b.Boot(ctx, cfg)
	st.Phases["machineStarted"] = time.Since(st.Started)
	if err != nil {
		return fail(fmt.Errorf("boot: %w", err))
	}
	guest := &proto.Client{Dial: m.DialAgent}
	var info proto.InfoResult
	deadline := time.Now().Add(5 * time.Minute)
	for {
		// Short attempts: before the agent listens, a vsock connect or a
		// forwarded TCP connection can hang instead of failing, and a long
		// timeout would delay readiness by up to that timeout.
		cctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		err = guest.Call(cctx, proto.OpInfo, nil, &info)
		cancel()
		if err == nil {
			break
		}
		select {
		case <-m.Done():
			return fail(fmt.Errorf("guest exited during boot (see %s)", cfg.ConsoleLog))
		case <-ctx.Done():
			m.Kill()
			return fail(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			m.Kill()
			return fail(fmt.Errorf("agent did not respond: %v (see %s)", err, cfg.ConsoleLog))
		}
	}
	st.Boot = time.Since(st.Started)
	st.Phases["agentReachable"] = st.Boot
	fmt.Fprintf(log, "guest ready in %s: kernel %s, %s\n", st.Boot.Round(time.Millisecond), info.Kernel, info.Runc)

	os.Remove(st.Socket)
	l, err := net.Listen("unix", st.Socket)
	if err != nil {
		m.Kill()
		return fail(err)
	}
	owner.chown(st.Socket)
	os.Chmod(st.Socket, 0o600)
	st.Ready = true
	st.Phases["socketReady"] = time.Since(st.Started)
	if err := writeState(cfg.Dir, st, owner); err != nil {
		m.Kill()
		return err
	}

	pool := newConnPool(ctx, m, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go proxy(ctx, c, pool)
		}
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintln(log, "stopping guest")
		l.Close()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := guest.Call(sctx, proto.OpShutdown, nil, nil)
		cancel()
		if err == nil {
			select {
			case <-m.Done():
			case <-time.After(20 * time.Second):
				m.Kill()
			}
		} else {
			m.Kill()
		}
	case <-m.Done():
		l.Close()
		fmt.Fprintln(log, "guest exited")
	}
	os.Remove(st.Socket)
	os.Remove(filepath.Join(cfg.Dir, "state.json"))
	return nil
}

func proxy(ctx context.Context, c net.Conn, pool *connPool) {
	defer c.Close()
	g, err := pool.get(ctx)
	if err != nil {
		return
	}
	defer g.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(g, c); closeWrite(g); done <- struct{}{} }()
	go func() { io.Copy(c, g); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	type cw interface{ CloseWrite() error }
	if x, ok := c.(cw); ok {
		x.CloseWrite()
	}
}

// ErrUnsupported is returned by backends that exist only as extension points.
var ErrUnsupported = errors.New("backend not supported on this host")

// GuestImage locates the kernel and initramfs for an architecture.
func GuestImage(home, arch, override string) (kernel, initrd string, err error) {
	var dirs []string
	if override != "" {
		dirs = append(dirs, override)
	}
	if d := os.Getenv("OAR_GUEST_DIR"); d != "" {
		dirs = append(dirs, filepath.Join(d, arch), d)
	}
	dirs = append(dirs, filepath.Join(home, "guest", arch))
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "..", "share", "oar", "guest", arch),
			filepath.Join(filepath.Dir(exe), "guest", arch))
	}
	dirs = append(dirs, filepath.Join("guest", "out", arch))
	for _, d := range dirs {
		k, i := filepath.Join(d, "kernel"), filepath.Join(d, "initramfs.cpio.gz")
		if fileExists(k) && fileExists(i) {
			ka, _ := filepath.Abs(k)
			ia, _ := filepath.Abs(i)
			return ka, ia, nil
		}
	}
	return "", "", fmt.Errorf("guest image for %s not found (looked in %s); build it with guest/build.sh %s or set OAR_GUEST_DIR",
		arch, strings.Join(dirs, ", "), arch)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// EnsureDisk creates a sparse raw disk image if it does not exist.
func EnsureDisk(p string, gib int) error {
	if fileExists(p) {
		return nil
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(int64(gib) << 30)
}
