//go:build linux

package proto

import (
	"net"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

// vsockBufferSize replaces the 256 KiB default vsock buffer. The receiver
// advertises it as flow-control credit, so it bounds how much data is in
// flight per connection: 4 MiB gave +35% throughput on vsock loopback.
const vsockBufferSize = 4 << 20

// TuneVsock enlarges the buffer of a vsock connection (other connections
// are left alone). Both ends should call it.
func TuneVsock(c net.Conn) {
	vc, ok := c.(*vsock.Conn)
	if !ok {
		return
	}
	rc, err := vc.SyscallConn()
	if err != nil {
		return
	}
	rc.Control(func(fd uintptr) {
		unix.SetsockoptUint64(int(fd), unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_MAX_SIZE, vsockBufferSize)
		unix.SetsockoptUint64(int(fd), unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_SIZE, vsockBufferSize)
	})
}

// TunedVsockListener applies TuneVsock to every accepted connection.
type TunedVsockListener struct{ net.Listener }

func (l TunedVsockListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		TuneVsock(c)
	}
	return c, err
}
