//go:build unix

package vm

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// peekClosed checks a socket for pending EOF without blocking or consuming
// data; ok is false when the connection exposes no socket.
func peekClosed(c net.Conn) (closed, ok bool) {
	sc, isSys := c.(syscall.Conn)
	if !isSys {
		return false, false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return false, false
	}
	var n int
	var rerr error
	if err := rc.Read(func(fd uintptr) bool {
		var b [1]byte
		n, _, rerr = unix.Recvfrom(int(fd), b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		return true // never wait
	}); err != nil {
		return false, false
	}
	switch {
	case rerr == unix.EAGAIN || rerr == unix.EWOULDBLOCK:
		return false, true // open, nothing pending
	case rerr != nil:
		return true, true
	default:
		return n == 0, true // 0 bytes: orderly shutdown
	}
}
