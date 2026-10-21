package vm

import (
	"net"
	"testing"
	"time"
)

func TestAlive(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	dial := func() (net.Conn, net.Conn) {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		s, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		return c, s
	}

	// socket access: non-blocking peek
	c, s := dial()
	defer c.Close()
	if !alive(c) {
		t.Fatal("open connection reported dead")
	}
	s.Close()
	time.Sleep(10 * time.Millisecond) // the FIN arrives
	if _, ok := peekClosed(c); !ok {
		t.Skip("no non-blocking peek on this platform: connections are assumed open")
	}
	if alive(c) {
		t.Fatal("closed connection reported alive")
	}

	// no socket access (like VZ): assumed open
	if !alive(struct{ net.Conn }{c}) {
		t.Fatal("connection without socket access reported dead")
	}
}
