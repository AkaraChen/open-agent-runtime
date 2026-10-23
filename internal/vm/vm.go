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
	// GuestCmdline is appended to the guest kernel command line
	// (oar vm start --guest-cmdline, for debugging).
	GuestCmdline string `json:"guestCmdline,omitempty"`
}

// guestKernelParams is what every backend puts on the guest kernel command
// line after the console device.
//
// PERF: rcupdate.rcu_expedited=1 turns every synchronize_rcu() in the guest
// from a grace period (3 to 10 ms) into a few hundred microseconds. runc
// create (and runc exec) wait for one: the container init joins its cgroup
// through a cgroup.procs write, which takes the cgroup threadgroup rwsem for
// writing, and rcu_sync_enter() on it waits a grace period unless another
// cgroup attach ran a few ms earlier. Measured (docs/research/runc-create-
// profile.md): that write is 5.4 ms median (3 to 15) of a 25.5 ms runc
// create on a 8 vCPU Linux 7.0 host and 0.09 ms with the parameter; runc
// create wall time 27.7 -> 19.7 ms (1.5.2) and 26.6 -> 18.8 ms (1.4.0),
// n=60 interleaved; the same write 8.8 -> 3.5 ms in this guest kernel under
// QEMU TCG. The cost is an IPI to every vCPU per grace period. Pass
// rcupdate.rcu_expedited=0 to --guest-cmdline to switch it off (A/B).
const guestKernelParams = "panic=-1 quiet loglevel=3 rcupdate.rcu_expedited=1"

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

// Home is the oar state directory: $XDG_DATA_HOME/oar, by default
// ~/.local/share/oar (%LOCALAPPDATA%\oar on Windows).
func Home() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "oar")
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
	// Transport is the control channel: "vsock", "tcp" (a port forward)
	// or "inprocess"; clients use it to decide whether to compress.
	Transport string `json:"transport,omitempty"`
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
	os.Remove(cfg.ConsoleLog) // the previous boot's markers must not match
	stopWatch := watchConsole(cfg.ConsoleLog, st.Started)
	defer stopWatch()
	m, err := b.Boot(ctx, cfg)
	st.Phases["machineStarted"] = time.Since(st.Started)
	if t, ok := m.(interface{ Transport() string }); ok {
		st.Transport = t.Transport()
	}
	if err != nil {
		return fail(fmt.Errorf("boot: %w", err))
	}
	guest := &proto.Client{Dial: m.DialAgent}
	var info proto.InfoResult
	deadline := time.Now().Add(5 * time.Minute)
	var ready <-chan struct{}
	if r, ok := m.(interface{ Ready() <-chan struct{} }); ok {
		ready = r.Ready()
	}
	listening := false // the agent reported that it listens
	for {
		// Short attempts: before the agent listens, a vsock connect or a
		// forwarded TCP connection can hang instead of failing, and a long
		// timeout would delay readiness by up to that timeout.
		// 200 ms attempts at first; slow (emulated) guests get longer ones
		attempt := 200 * time.Millisecond
		if time.Since(st.Started) > 20*time.Second {
			attempt = time.Second
		}
		if listening {
			// nothing hangs any more; the first call may just be slow
			attempt = 5 * time.Second
		}
		cctx, cancel := context.WithTimeout(ctx, attempt)
		readied := false
		if ready != nil {
			// the agent's report cancels a probe that is hanging in the
			// transport; the next one connects at once
			stop, fired := make(chan struct{}), make(chan bool, 1)
			go func(ready <-chan struct{}) {
				select {
				case <-ready:
					cancel()
					fired <- true
				case <-stop:
					fired <- false
				}
			}(ready)
			err = guest.Call(cctx, proto.OpInfo, nil, &info)
			close(stop)
			readied = <-fired
		} else {
			err = guest.Call(cctx, proto.OpInfo, nil, &info)
		}
		cancel()
		if err == nil {
			break
		}
		if readied {
			ready, listening = nil, true
			continue
		}
		select {
		case <-m.Done():
			if e, ok := m.(interface{ ExitErr() error }); ok && e.ExitErr() != nil {
				return fail(fmt.Errorf("guest exited during boot: %w (see %s)", e.ExitErr(), cfg.ConsoleLog))
			}
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
	for phase, at := range stopWatch() {
		st.Phases[phase] = at
	}
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
	if b.NeedsGuestImage() {
		go forwardIngress(ctx, &proto.Client{Dial: pool.get}, m, "127.0.0.1", log)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go proxy(ctx, c, pool, log)
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
				CloseDataDisk(cfg.Dir) // the guest synced and unmounted before powering off
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

func proxy(ctx context.Context, c net.Conn, pool *connPool, log io.Writer) {
	defer c.Close()
	// a failed connect used to close the client's connection silently (it
	// saw "read response: EOF"); transient failures, as VZ produces right
	// after boot, are retried and lasting ones logged
	var g net.Conn
	var err error
	for attempt, delay := 0, 20*time.Millisecond; attempt < 6; attempt, delay = attempt+1, delay*2 {
		if g, err = pool.get(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
	if err != nil {
		fmt.Fprintf(log, "connect to the guest agent: %v\n", err)
		return
	}
	proto.Pipe(c, g)
}

// ErrUnsupported is returned by backends that exist only as extension points.
var ErrUnsupported = errors.New("backend not supported on this host")

// GuestImage locates the kernel and initramfs for an architecture.
func GuestImage(home, arch, override string) (kernel, initrd string, err error) {
	var dirs []string
	if override != "" {
		dirs = append(dirs, override)
	}
	dirs = append(dirs, filepath.Join(home, "guest", arch))
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "..", "share", "oar", "guest", arch),
			filepath.Join(filepath.Dir(exe), "guest", arch),
			filepath.Join(filepath.Dir(exe), "..", "guest", "out", arch)) // bin/oar of a checkout
	}
	dirs = append(dirs, filepath.Join("guest", "out", arch))
	for _, d := range dirs {
		k := filepath.Join(d, "kernel")
		// LZ4 since step 6 of the benchmark roadmap (faster to unpack);
		// gzip images built earlier still boot
		for _, name := range []string{"initramfs.cpio.lz4", "initramfs.cpio.gz"} {
			i := filepath.Join(d, name)
			if fileExists(k) && fileExists(i) {
				ka, _ := filepath.Abs(k)
				ia, _ := filepath.Abs(i)
				return ka, ia, nil
			}
		}
	}
	return "", "", fmt.Errorf("guest image for %s not found (looked in %s); build it with guest/build.sh %s or pass --guest-dir",
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

// The guest's data disk is a rebuildable cache, so the backends attach it
// without durability: a guest flush does not reach the host's disk. After a
// host crash or a killed VM the image can be inconsistent in ways no fsck
// repairs, and nothing in it is worth repairing. A marker file next to the
// image records that a VM may be writing to it. It is created (and made
// durable) before the guest boots and removed once the guest has shut down
// cleanly and the image has been synced; a marker found at the next boot
// means the image is discarded and the guest formats a fresh one.
const diskMarker = "data.img.open"

// OpenDataDisk returns the path of the data disk in dir, creating it, and
// starts the unclean-shutdown window described above.
func OpenDataDisk(dir string, gib int) (string, error) {
	disk := filepath.Join(dir, "data.img")
	if fileExists(filepath.Join(dir, diskMarker)) {
		os.Remove(disk)
	}
	if err := EnsureDisk(disk, gib); err != nil {
		return "", err
	}
	f, err := os.Create(filepath.Join(dir, diskMarker))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return "", err
	}
	// the marker must be durable before the first write to the image
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return disk, nil
}

// CloseDataDisk ends the window after a clean guest shutdown. The image is
// written to the host's disk first, so a later host crash cannot leave it
// torn behind a missing marker.
func CloseDataDisk(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "data.img"), os.O_WRONLY, 0)
	if err != nil {
		return
	}
	err = f.Sync()
	f.Close()
	if err == nil {
		os.Remove(filepath.Join(dir, diskMarker))
	}
}

// consoleMarkers are guest init messages whose arrival on the host is
// recorded as boot phases: they split the boot into the part before the
// guest's own clock starts (VMM, firmware, kernel decompression), the
// guest's boot, and the delay until the host reaches the agent.
var consoleMarkers = map[string]string{
	"[oar-init] init started": "consoleInit",
	" listening on ":          "consoleAgent", // the agent accepts connections
}

// watchConsole polls the console log during boot for the markers. The
// returned function stops it and returns when each marker first appeared.
func watchConsole(path string, start time.Time) func() map[string]time.Duration {
	done := make(chan struct{})
	finished := make(chan struct{})
	seen := map[string]time.Duration{}
	go func() {
		defer close(finished)
		var off int64
		var partial string
		for {
			if f, err := os.Open(path); err == nil {
				f.Seek(off, io.SeekStart)
				b, _ := io.ReadAll(f)
				f.Close()
				off += int64(len(b))
				now := time.Since(start)
				lines := strings.Split(partial+string(b), "\n")
				partial = lines[len(lines)-1]
				for _, line := range lines[:len(lines)-1] {
					for marker, phase := range consoleMarkers {
						if _, ok := seen[phase]; !ok && strings.Contains(line, marker) {
							seen[phase] = now
						}
					}
				}
				if len(seen) == len(consoleMarkers) {
					return
				}
			}
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	var once sync.Once
	return func() map[string]time.Duration {
		once.Do(func() { close(done) })
		<-finished
		return seen
	}
}
