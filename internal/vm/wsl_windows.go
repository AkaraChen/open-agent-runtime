//go:build windows

package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"
)

// wsl2 runs the guest as a dedicated WSL2 distribution imported from the
// guest rootfs tarball. WSL2 provides the Linux kernel; the agent listens
// on loopback TCP, which WSL2 forwards to the Windows host.
type wsl2 struct{}

func init() { Register(wsl2{}) }

func (wsl2) Name() string          { return "wsl2" }
func (wsl2) NeedsGuestImage() bool { return true }
func (wsl2) NeedsRoot() bool       { return false }

func (wsl2) Available() error {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return fmt.Errorf("wsl.exe not found; install WSL2")
	}
	return nil
}

func decodeWSL(b []byte) string {
	if len(b) >= 2 && b[1] == 0 { // UTF-16LE
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}

func distroExists(name string) bool {
	out, err := exec.Command("wsl.exe", "--list", "--quiet").Output()
	if err != nil {
		return false
	}
	for _, l := range strings.Split(decodeWSL(out), "\n") {
		if strings.TrimSpace(strings.Trim(l, "\x00\r")) == name {
			return true
		}
	}
	return false
}

type wslMachine struct {
	distro string
	addr   string
	cmd    *exec.Cmd
	done   chan struct{}
	once   sync.Once
}

func (wsl2) Boot(ctx context.Context, cfg MachineConfig) (Machine, error) {
	distro := "oar-" + cfg.Name
	if !distroExists(distro) {
		if cfg.RootFSTar == "" {
			return nil, fmt.Errorf("guest rootfs.tar not found; build it with guest/build.sh")
		}
		dest := filepath.Join(cfg.Dir, "wsl")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return nil, err
		}
		out, err := exec.Command("wsl.exe", "--import", distro, dest, cfg.RootFSTar, "--version", "2").CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("wsl --import: %v: %s", err, decodeWSL(out))
		}
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	logf, err := os.OpenFile(cfg.ConsoleLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("wsl.exe", "-d", distro, "-u", "root", "--", "/init", "serve",
		"--mode", "wsl", "--listen", "tcp:"+addr, "--root", "/var/lib/oar")
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	m := &wslMachine{distro: distro, addr: addr, cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		logf.Close()
		m.once.Do(func() { close(m.done) })
	}()
	return m, nil
}

func (m *wslMachine) DialAgent(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", m.addr)
}

func (m *wslMachine) Done() <-chan struct{} { return m.done }

func (m *wslMachine) Kill() error {
	exec.Command("wsl.exe", "--terminate", m.distro).Run()
	if m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
	<-m.done
	return nil
}
