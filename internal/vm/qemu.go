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
	"time"
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
	if b := os.Getenv("OAR_QEMU"); b != "" {
		return b
	}
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
	if a := os.Getenv("OAR_QEMU_ACCEL"); a != "" {
		return a
	}
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
	disk := filepath.Join(cfg.Dir, "data.img")
	if err := EnsureDisk(disk, cfg.DiskGiB); err != nil {
		return nil, err
	}
	accel := accelerator(arch)
	var machine, console string
	cpu := "max"
	if accel != "tcg" {
		cpu = "host"
	}
	switch arch {
	case "amd64":
		machine, console = "q35,accel="+accel, "ttyS0"
	case "arm64":
		machine, console = "virt,accel="+accel, "ttyAMA0"
		if accel != "hvf" {
			machine += ",gic-version=max"
		}
	default:
		return nil, fmt.Errorf("unsupported arch %s", arch)
	}
	useVsock := vsockAvailable()
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		m := &qemuMachine{done: make(chan struct{})}
		cmdline := fmt.Sprintf("console=%s panic=-1 quiet loglevel=3", console)
		netdev := "user,id=net0"
		var transport []string
		if useVsock {
			m.cid = uint32(3 + rand.IntN(1<<30))
			transport = []string{"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", m.cid)}
		} else {
			port, err := freePort()
			if err != nil {
				return nil, err
			}
			m.tcpAddr = fmt.Sprintf("127.0.0.1:%d", port)
			netdev += fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:1024", port)
			cmdline += " oar.tcp=1024"
		}
		args := []string{
			"-machine", machine, "-cpu", cpu,
			"-smp", strconv.Itoa(cfg.CPUs), "-m", strconv.Itoa(cfg.MemoryMiB),
			"-nodefaults", "-no-user-config", "-nographic", "-no-reboot",
			"-kernel", cfg.Kernel, "-initrd", cfg.Initrd,
			"-append", cmdline,
			"-serial", "file:" + cfg.ConsoleLog,
			"-drive", "if=none,id=data,format=raw,file=" + disk + ",cache=writeback",
			"-device", "virtio-blk-pci,drive=data",
			"-netdev", netdev, "-device", "virtio-net-pci,netdev=net0",
			"-device", "virtio-rng-pci",
			"-monitor", "none",
		}
		args = append(args, transport...)
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
			return nil, err
		}
		m.cmd = cmd
		go func() {
			m.err = cmd.Wait()
			logf.Close()
			m.once.Do(func() { close(m.done) })
		}()
		select {
		case <-m.done:
			b, _ := os.ReadFile(logPath)
			lastErr = fmt.Errorf("qemu exited: %v: %s", m.err, strings.TrimSpace(string(b)))
			if useVsock && strings.Contains(string(b), "vhost-vsock") {
				continue // CID clash
			}
			return nil, lastErr
		case <-time.After(300 * time.Millisecond):
		}
		return m, nil
	}
	return nil, lastErr
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
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (m *qemuMachine) Done() <-chan struct{} { return m.done }

func (m *qemuMachine) Kill() error {
	if m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
	<-m.done
	return nil
}
