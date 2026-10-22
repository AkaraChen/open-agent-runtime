package vm

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// qemu boots the guest with QEMU. On Linux it uses KVM when /dev/kvm is
// usable and talks to the agent over virtio-vsock; elsewhere (or without
// vhost-vsock) it forwards a loopback TCP port into the guest. Without a
// hardware accelerator it falls back to TCG emulation, which also allows
// running foreign-architecture guests.
type qemu struct{ name string }

func init() {
	Register(qemu{name: "qemu"})
	Register(qemu{name: "kvm"})
}

func (q qemu) Name() string        { return q.name }
func (qemu) NeedsGuestImage() bool { return true }
func (qemu) NeedsRoot() bool       { return false }

func qemuBinary(arch string) string {
	if arch == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

func (qemu) Available() error {
	if _, err := exec.LookPath(qemuBinary(runtime.GOARCH)); err != nil {
		return fmt.Errorf("%s not found (install qemu)", qemuBinary(runtime.GOARCH))
	}
	return nil
}

// accelerator picks kvm/hvf/whpx when usable for arch, else tcg.
func accelerator(arch string) string {
	if arch != runtime.GOARCH {
		return "tcg"
	}
	switch runtime.GOOS {
	case "linux":
		if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
			f.Close()
			return "kvm"
		}
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "kern.hv_support").Output(); err == nil && strings.TrimSpace(string(out)) == "1" {
			return "hvf"
		}
	}
	return "tcg"
}

type qemuMachine struct {
	cmd     *exec.Cmd
	cid     uint32
	tcpAddr string
	done    chan struct{}
	once    sync.Once
	err     error
	ready   chan struct{} // closed when the guest agent reports it listens
	qmp     *qmpClient
}

// HostForward makes QEMU listen on 127.0.0.1:port and forward to the same
// guest port (user networking). Only with the TCP transport: over vsock the
// control channel is faster than user networking (slirp).
func (m *qemuMachine) HostForward(port int) error {
	if m.qmp == nil {
		return fmt.Errorf("no QMP socket")
	}
	if m.tcpAddr == "" {
		return fmt.Errorf("vsock transport: the control channel is used")
	}
	return m.qmp.hmp(fmt.Sprintf("hostfwd_add net0 tcp:127.0.0.1:%d-:%d", port, port))
}

func (m *qemuMachine) HostUnforward(port int) {
	if m.qmp != nil {
		m.qmp.hmp(fmt.Sprintf("hostfwd_remove net0 tcp:127.0.0.1:%d", port))
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (q qemu) Boot(ctx context.Context, cfg MachineConfig) (Machine, error) {
	arch := cfg.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	disk, err := OpenDataDisk(cfg.Dir, cfg.DiskGiB)
	if err != nil {
		return nil, err
	}
	accel := accelerator(arch)
	var machine, console string
	// device suffix: virtio over PCI, or virtio-mmio on microvm
	bus := "-pci"
	cpu := "max"
	if accel != "tcg" {
		cpu = "host"
	}
	switch arch {
	case "amd64":
		// microvm (no PCI bus, minimal ACPI) boots in about half the time of
		// q35, whose PCI and ACPI enumeration dominated the kernel's boot.
		// It has no HPET or ACPI PM timer, so the kernel needs kvm-clock to
		// learn the TSC frequency: without KVM, a loaded host can make PIT
		// calibration fail and the boot hang.
		machine, console = "q35,accel="+accel, "ttyS0"
		if accel == "kvm" {
			// devices on microvm's PCIe host bridge (virtio-pci with MSI):
			// guest connections take half as long as with virtio-mmio's
			// legacy interrupts (0.10 vs 0.23 ms per connect) for 12 ms of
			// kernel boot
			machine, bus = "microvm,accel="+accel+",pcie=on,rtc=on", "-pci"
		}
	case "arm64":
		machine, console = "virt,accel="+accel, "ttyAMA0"
		if accel != "hvf" {
			machine += ",gic-version=max"
		}
	default:
		return nil, fmt.Errorf("unsupported arch %s", arch)
	}
	useVsock := vsockAvailable()
	m := &qemuMachine{done: make(chan struct{})}
	cmdline := fmt.Sprintf("console=%s panic=-1 quiet loglevel=3", console)
	// PERF: the guest's devices are all virtio and address every byte of
	// memory, so the kernel needs no swiotlb bounce buffer (64 MiB, allocated
	// and zeroed at boot once the guest has memory above 4 GiB).
	// KASLR stays: nokaslr would save only a relocation pass before the
	// kernel's clock starts, and KASLR hides the guest kernel's layout from
	// the code the agent runs.
	cmdline += " swiotlb=noforce"
	if arch == "amd64" {
		// PERF: skips the early PCI quirk scan, about 140 port accesses (VM
		// exits) that probe bus 0 for chipsets a VM does not have
		cmdline += " pci=noearly"
	}
	if cfg.GuestCmdline != "" {
		cmdline += " " + cfg.GuestCmdline
	}
	// QEMU user networking always hands out the same addresses
	cmdline += " oar.net=10.0.2.15/24,10.0.2.2,10.0.2.3"
	netdev := "user,id=net0"
	var transport []string
	if useVsock {
		m.cid = uint32(3 + rand.IntN(1<<30))
		transport = []string{"-device", fmt.Sprintf("vhost-vsock%s,guest-cid=%d", bus, m.cid)}
	} else {
		port, err := freePort()
		if err != nil {
			return nil, err
		}
		m.tcpAddr = fmt.Sprintf("127.0.0.1:%d", port)
		netdev += fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:1024", port)
		cmdline += " oar.tcp=1024"
		// the agent connects back when it listens (10.0.2.2 is the host's
		// loopback in QEMU user networking): probes through the port
		// forward hang until it does, instead of failing
		if l, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
			m.ready = make(chan struct{})
			cmdline += fmt.Sprintf(" oar.notify=10.0.2.2:%d", l.Addr().(*net.TCPAddr).Port)
			go func() {
				defer l.Close()
				go func() { <-m.done; l.Close() }()
				if c, err := l.Accept(); err == nil {
					c.Close()
					close(m.ready)
				}
			}()
		}
	}
	netDevice := "virtio-net" + bus + ",netdev=net0"
	if bus == "-pci" {
		netDevice += ",romfile=" // no PXE option ROM
	}
	args := []string{
		"-machine", machine, "-cpu", cpu,
		"-smp", strconv.Itoa(cfg.CPUs), "-m", strconv.Itoa(cfg.MemoryMiB),
		"-nodefaults", "-no-user-config", "-nographic", "-no-reboot",
		"-kernel", cfg.Kernel, "-initrd", cfg.Initrd,
		"-append", cmdline,
		"-serial", "file:" + cfg.ConsoleLog,
		"-drive", "if=none,id=data,format=raw,file=" + disk + ",cache=unsafe",
		"-device", "virtio-blk" + bus + ",drive=data",
		"-netdev", netdev, "-device", netDevice,
		"-device", "virtio-rng" + bus,
		"-monitor", "none",
	}
	args = append(args, transport...)
	// QMP for runtime port forwarding; a short private directory keeps the
	// socket path within the unix socket limit (104 bytes on macOS)
	if qdir, err := os.MkdirTemp("/tmp", "oar-qmp-"); err == nil {
		sock := filepath.Join(qdir, "qmp.sock")
		args = append(args, "-qmp", "unix:"+sock+",server=on,wait=off")
		m.qmp = &qmpClient{path: sock}
		go func() { <-m.done; os.RemoveAll(qdir) }()
	}
	cmd := exec.Command(qemuBinary(arch), args...)
	logPath := filepath.Join(cfg.Dir, "qemu.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	setDeathSignal(cmd)
	if err := cmd.Start(); err != nil {
		logf.Close()
		m.once.Do(func() { close(m.done) }) // releases the QMP directory
		return nil, err
	}
	m.cmd = cmd
	go func() {
		err := cmd.Wait()
		logf.Close()
		b, _ := os.ReadFile(logPath)
		m.err = fmt.Errorf("qemu exited: %v: %s", err, strings.TrimSpace(string(b)))
		m.once.Do(func() { close(m.done) })
	}()
	// Return at once: the supervisor polls the agent and watches Done,
	// so early qemu failures still surface (with qemu's output). CIDs
	// are random in a 2^30 space, so clashes are not retried.
	return m, nil
}

func (m *qemuMachine) DialAgent(ctx context.Context) (net.Conn, error) {
	if m.tcpAddr != "" {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", m.tcpAddr)
	}
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := dialVsock(m.cid, 1024)
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.err == nil && r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (m *qemuMachine) Done() <-chan struct{} { return m.done }

// Transport reports the control channel.
func (m *qemuMachine) Transport() string {
	if m.tcpAddr != "" {
		return "tcp"
	}
	return "vsock"
}

// Ready is closed when the guest agent reports that it listens (nil when
// the transport needs no report).
func (m *qemuMachine) Ready() <-chan struct{} { return m.ready }

// ExitErr describes why qemu exited (valid after Done is closed).
func (m *qemuMachine) ExitErr() error { return m.err }

func (m *qemuMachine) Kill() error {
	if m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
	<-m.done
	return nil
}
