//go:build windows

package cli

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// terminate kills the supervisor; Windows has no SIGTERM, so the guest is
// torn down by the backend-specific cleanup in the supervisor's children.
func terminate(pid int, _ bool) error {
	exec.Command("taskkill", "/PID", itoa(pid), "/T", "/F").Run()
	return nil
}

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
