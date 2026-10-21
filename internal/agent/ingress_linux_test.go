//go:build linux

package agent

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// echoServer answers every line with "<name>\n".
func echoServer(t *testing.T, name string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					if _, err := r.ReadString('\n'); err != nil {
						return
					}
					c.Write([]byte(name + "\n"))
				}
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func ask(t *testing.T, c net.Conn) string {
	c.Write([]byte("hi\n"))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func TestIngressSwitchAndDrain(t *testing.T) {
	s := &Server{cfg: Config{Mode: "native"}}
	pub := freePort(t)
	ports := []manifest.Port{{Name: "http", Port: pub, Env: "PORT"}}
	a := &instance{Name: "svc", ID: "svc-a", Ports: map[string]int{"http": echoServer(t, "a")}, PublicPorts: ports}
	b := &instance{Name: "svc", ID: "svc-b", Ports: map[string]int{"http": echoServer(t, "b")}, PublicPorts: ports}
	if _, err := s.route(a); err != nil {
		t.Fatal(err)
	}
	defer s.closeAllIngress()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(pub))
	held, err := net.Dial("tcp", addr) // a long-lived connection to a
	if err != nil {
		t.Fatal(err)
	}
	if got := ask(t, held); got != "a" {
		t.Fatalf("got %q", got)
	}
	olds, err := s.route(b)
	if err != nil || len(olds) != 1 || olds[0].instance != "svc-a" {
		t.Fatalf("olds %v err %v", olds, err)
	}
	c, _ := net.Dial("tcp", addr)
	if got := ask(t, c); got != "b" {
		t.Fatalf("new connection went to %q, want b", got)
	}
	c.Close()
	if got := ask(t, held); got != "a" {
		t.Fatalf("existing connection moved to %q", got)
	}
	// drain waits for the held connection
	if d := drain(context.Background(), olds, 100*time.Millisecond); d < 90*time.Millisecond {
		t.Fatalf("drain returned early (%s) with a connection in flight", d)
	}
	held.Close()
	time.Sleep(20 * time.Millisecond)
	if d := drain(context.Background(), olds, time.Second); d > 100*time.Millisecond {
		t.Fatalf("drain took %s after the connection closed", d)
	}
	s.unroute("svc", "svc-b")
	if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		t.Fatal("public port still open after unroute")
	}
}

func TestIngressListLongPoll(t *testing.T) {
	s := &Server{cfg: Config{Mode: "native"}}
	defer s.closeAllIngress()
	v0 := s.ingressList().Version
	// a stale version answers at once
	if l := s.ingressWait(context.Background(), proto.IngressListArgs{Version: v0 + 1, Wait: time.Minute}); l.Version != v0 {
		t.Fatalf("version %d, want %d", l.Version, v0)
	}
	// the current version waits until the list changes
	got := make(chan proto.IngressList)
	go func() {
		got <- s.ingressWait(context.Background(), proto.IngressListArgs{Version: v0, Wait: time.Minute})
	}()
	select {
	case <-got:
		t.Fatal("returned before a change")
	case <-time.After(50 * time.Millisecond):
	}
	pub := freePort(t)
	in := &instance{Name: "svc", ID: "svc-a", Ports: map[string]int{"http": echoServer(t, "a")}, PublicPorts: []manifest.Port{{Name: "http", Port: pub}}}
	if _, err := s.route(in); err != nil {
		t.Fatal(err)
	}
	select {
	case l := <-got:
		if l.Version == v0 || len(l.Ingresses) != 1 || l.Ingresses[0].Port != pub {
			t.Fatalf("got %+v", l)
		}
	case <-time.After(time.Second):
		t.Fatal("not woken by route")
	}
	// and gives up after Wait
	start := time.Now()
	s.ingressWait(context.Background(), proto.IngressListArgs{Version: s.ingressList().Version, Wait: 30 * time.Millisecond})
	if d := time.Since(start); d < 30*time.Millisecond || d > time.Second {
		t.Fatalf("waited %s", d)
	}
}
