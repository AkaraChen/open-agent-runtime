//go:build linux

package vm

import (
	"net"
	"os"
	"os/exec"
	"syscall"

	"github.com/mdlayher/vsock"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// vsockAvailable reports whether QEMU can use vhost-vsock; otherwise the
// control channel is a TCP port forward.
func vsockAvailable() bool {
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
	proto.TuneVsock(c)
	return c, nil
}

func setDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
