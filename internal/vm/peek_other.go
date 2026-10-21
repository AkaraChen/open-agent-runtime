//go:build !unix

package vm

import "net"

func peekClosed(net.Conn) (closed, ok bool) { return false, false }
