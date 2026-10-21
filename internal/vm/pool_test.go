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
	if alive(c) {
		t.Fatal("closed connection reported alive")
	}

	// no socket access (like VZ): short-deadline read
	c2, s2 := dial()
	defer c2.Close()
	w := struct{ net.Conn }{c2} // hides syscall.Conn
	if !alive(w) {
		t.Fatal("open wrapped connection reported dead")
	}
	s2.Close()
	time.Sleep(10 * time.Millisecond)
	if alive(w) {
		t.Fatal("closed wrapped connection reported alive")
	}
}
