//go:build !windows

package cli

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func terminate(pid int, privileged bool) error {
	err := syscall.Kill(pid, syscall.SIGTERM)
	if err == syscall.EPERM && privileged {
		c := exec.Command("sudo", "kill", "-TERM", itoa(pid))
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}
	return err
}

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
