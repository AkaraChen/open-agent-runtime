//go:build !linux

package vm

import (
	"fmt"
	"net"
	"os/exec"
)

func vsockAvailable() bool { return false }

func dialVsock(cid, port uint32) (net.Conn, error) {
	return nil, fmt.Errorf("vsock is not available on this host")
}

func setDeathSignal(cmd *exec.Cmd) {}
