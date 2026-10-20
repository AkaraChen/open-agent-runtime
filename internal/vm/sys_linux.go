//go:build linux

package vm

import (
	"net"
	"os"
	"os/exec"
	"syscall"

	"github.com/mdlayher/vsock"
)

func vsockAvailable() bool {
	if os.Getenv("OAR_QEMU_TRANSPORT") == "tcp" {
		return false
	}
	f, err := os.OpenFile("/dev/vhost-vsock", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func dialVsock(cid, port uint32) (net.Conn, error) {
	c, err := vsock.Dial(cid, port, nil)
	if err != nil {
		return nil, err // not a typed nil *vsock.Conn inside net.Conn
	}
	return c, nil
}

func setDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
