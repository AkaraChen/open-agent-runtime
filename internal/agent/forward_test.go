//go:build linux

package agent_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mdlayher/vsock"

	"github.com/AkaraChen/open-agent-runtime/internal/agent"
	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/proto"
	"github.com/AkaraChen/open-agent-runtime/internal/vm"
)

// The ingress benchmarks measure what a client on the host sees:
//
//	direct:   client -> app (baseline, no oar involved)
//	native:   client -> ingress proxy -> app
//	vm-tcp:   client -> host forwarder -> TCP control channel -> agent -> app
//	vm-vsock: the same over AF_VSOCK loopback (modprobe vsock_loopback),
//	          the transport the kvm backend uses
//	vm-slow:  vm-tcp with 1 ms one-way delay on the control channel
//	          (2 ms round trip, a stand-in for slow vz/qemu channels)
//
// connect opens a connection per request; download/upload move 64 MiB
// through one connection.

// payload is what "get" sends, repeated.
var payload = func() []byte {
	b := make([]byte, 1<<20)
	for i := range b {
		b[i] = byte(i * 7 / 3)
	}
	return b
}()

// appServer answers "ping" with "pong", "get N" with N bytes of payload,
// "put N" by reading N bytes and answering "ok", and "sum N" by reading N
// bytes and answering their sha256.
func appServer(tb testing.TB, name string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { l.Close() })
	chunk := payload
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
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
					n, _ := strconv.ParseInt(arg, 10, 64)
					switch cmd {
					case "ping":
						c.Write([]byte("pong\n"))
					case "name":
						c.Write([]byte(name + "\n"))
					case "get":
						for n > 0 {
							w, err := c.Write(chunk[:min(n, int64(len(chunk)))])
							if err != nil {
								return
							}
							n -= int64(w)
						}
					case "put":
						io.CopyN(io.Discard, r, n)
						c.Write([]byte("ok\n"))
					case "sum":
						h := sha256.New()
						io.CopyN(h, r, n)
						fmt.Fprintf(c, "%x\n", h.Sum(nil))
					}
				}
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func freePort(tb testing.TB) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// guest starts an agent ("guest" side, native mode so its ingress listens
// on 127.0.0.1 only) and a host forwarder on 127.0.0.2 talking to it over
// the given control channel; a real VM has its own network stack.
func guest(tb testing.TB, kind string) *agent.Server {
	s := newAgent(tb)
	var l net.Listener
	var err error
	var dial proto.Dialer
	switch kind {
	case "vm-tcp", "vm-slow":
		l, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			tb.Fatal(err)
		}
		addr := l.Addr().String()
		dial = func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
		if kind == "vm-slow" {
			l = slowListener{l}
			fast := dial
			dial = func(ctx context.Context) (net.Conn, error) {
				c, err := fast(ctx)
				if err != nil {
					return nil, err
				}
				return newSlowConn(c), nil
			}
		}
	case "vm-vsock":
		vl, err := vsock.ListenContextID(vsock.Local, 0, nil)
		if err != nil {
			tb.Skipf("vsock loopback unavailable (modprobe vsock_loopback): %v", err)
		}
		port := vl.Addr().(*vsock.Addr).Port
		probe, err := vsock.Dial(vsock.Local, port, nil)
		if err != nil {
			vl.Close()
			tb.Skipf("vsock loopback unavailable (modprobe vsock_loopback): %v", err)
		}
		probe.Close()
		l = proto.TunedVsockListener{Listener: vl}
		dial = func(ctx context.Context) (net.Conn, error) {
			c, err := vsock.Dial(vsock.Local, port, nil)
			if err == nil {
				proto.TuneVsock(c)
			}
			return c, err
		}
	default:
		tb.Fatalf("unknown path %q", kind)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)
	go proto.Listen(ctx, l, s.Handle)
	go vm.ForwardIngress(ctx, &proto.Client{Dial: dial}, "127.0.0.2", io.Discard)
	return s
}

// newAgent creates an agent whose state lives in a temporary directory.
func newAgent(tb testing.TB) *agent.Server {
	dir := tb.TempDir()
	s, err := agent.New(agent.Config{Root: dir, Mode: "native", Runc: agent.Runc{Root: dir + "/runc"}})
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

func publicPorts(port int) []manifest.Port {
	return []manifest.Port{{Name: "http", Port: port, Env: "PORT"}}
}

// waitServing pings the app through addr until it answers and returns how
// long that took.
func waitServing(tb testing.TB, addr string) time.Duration {
	start := time.Now()
	buf := make([]byte, 5)
	for {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.SetDeadline(time.Now().Add(time.Second))
			c.Write([]byte("ping\n"))
			_, err := io.ReadFull(c, buf)
			c.Close()
			if err == nil {
				return time.Since(start)
			}
		}
		if time.Since(start) > 5*time.Second {
			tb.Fatalf("%s did not come up", addr)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// ingressPath sets up one of the paths above and returns the address the
// client dials.
func ingressPath(tb testing.TB, kind string) string {
	app := appServer(tb, "a")
	if kind == "direct" {
		return fmt.Sprintf("127.0.0.1:%d", app)
	}
	pub := freePort(tb)
	if kind == "native" {
		s := newAgent(tb)
		if _, err := s.Route("svc", "svc-a", map[string]int{"http": app}, publicPorts(pub)); err != nil {
			tb.Fatal(err)
		}
		return fmt.Sprintf("127.0.0.1:%d", pub)
	}
	s := guest(tb, kind)
	if _, err := s.Route("svc", "svc-a", map[string]int{"http": app}, publicPorts(pub)); err != nil {
		tb.Fatal(err)
	}
	addr := fmt.Sprintf("127.0.0.2:%d", pub)
	waitServing(tb, addr)
	return addr
}

// request sends line (if not empty) and reads one line of answer.
func request(t *testing.T, c net.Conn, r *bufio.Reader, line string) string {
	t.Helper()
	if line != "" {
		fmt.Fprintf(c, "%s\n", line)
	}
	got, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	return strings.TrimSpace(got)
}

func TestForward(t *testing.T) {
	for _, kind := range []string{"vm-tcp", "vm-vsock"} {
		t.Run(kind, func(t *testing.T) {
			s := guest(t, kind)
			pub := freePort(t)
			addr := fmt.Sprintf("127.0.0.2:%d", pub)

			// a routed port shows up on the host right away (long poll,
			// not the next 500 ms poll)
			if _, err := s.Route("svc", "svc-a", map[string]int{"http": appServer(t, "a")}, publicPorts(pub)); err != nil {
				t.Fatal(err)
			}
			if d := waitServing(t, addr); d > 200*time.Millisecond {
				t.Errorf("host port came up after %s", d)
			}

			// bytes arrive intact in both directions
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			r := bufio.NewReader(c)
			const n = 8 << 20
			fmt.Fprintf(c, "get %d\n", n)
			h := sha256.New()
			if _, err := io.CopyN(h, r, n); err != nil {
				t.Fatal(err)
			}
			want := sha256.New()
			for i := 0; i < n/len(payload); i++ {
				want.Write(payload)
			}
			if !bytes.Equal(h.Sum(nil), want.Sum(nil)) {
				t.Fatal("download corrupted")
			}
			fmt.Fprintf(c, "sum %d\n", n)
			for i := 0; i < n/len(payload); i++ {
				c.Write(payload)
			}
			if got := request(t, c, r, ""); got != fmt.Sprintf("%x", want.Sum(nil)) {
				t.Fatalf("upload corrupted: %s", got)
			}

			// switching: new connections go to b, c stays on a and
			// counts as in flight on a until it closes
			inFlight, err := s.Route("svc", "svc-b", map[string]int{"http": appServer(t, "b")}, publicPorts(pub))
			if err != nil {
				t.Fatal(err)
			}
			c2, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			if got := request(t, c2, bufio.NewReader(c2), "name"); got != "b" {
				t.Fatalf("new connection went to %q", got)
			}
			c2.Close()
			if got := request(t, c, r, "name"); got != "a" {
				t.Fatalf("existing connection moved to %q", got)
			}
			if got := inFlight(); got != 1 {
				t.Fatalf("old instance has %d connections in flight, want 1", got)
			}
			c.Close()
			for deadline := time.Now().Add(2 * time.Second); inFlight() != 0; {
				if time.Now().After(deadline) {
					t.Fatalf("old instance still has %d connections in flight", inFlight())
				}
				time.Sleep(time.Millisecond)
			}

			// an instance that does not listen: the client is
			// disconnected instead of hanging
			if _, err := s.Route("svc", "svc-c", map[string]int{"http": freePort(t)}, publicPorts(pub)); err != nil {
				t.Fatal(err)
			}
			c3, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			c3.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(c3, "ping\n")
			if _, err := c3.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("read from a dead target: %v", err)
			}
			c3.Close()

			// unrouted ports disappear from the host
			s.Unroute("svc", "svc-c")
			for deadline := time.Now().Add(2 * time.Second); ; {
				c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
				if err != nil {
					break
				}
				c.Close()
				if time.Now().After(deadline) {
					t.Fatal("host port still open after unroute")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

// slowConn delivers every write a millisecond late without blocking the
// writer (propagation delay, not a bandwidth limit).
type slowConn struct {
	net.Conn
	q    chan []byte
	done chan struct{}
}

func newSlowConn(c net.Conn) *slowConn {
	s := &slowConn{Conn: c, q: make(chan []byte, 1024), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for p := range s.q {
			time.Sleep(time.Until(time.Unix(0, int64(binary.BigEndian.Uint64(p)))))
			if _, err := c.Write(p[8:]); err != nil {
				return
			}
		}
	}()
	return s
}

func (s *slowConn) Write(p []byte) (int, error) {
	b := make([]byte, 8+len(p))
	binary.BigEndian.PutUint64(b, uint64(time.Now().Add(time.Millisecond).UnixNano()))
	copy(b[8:], p)
	s.q <- b
	return len(p), nil
}

func (s *slowConn) CloseWrite() error {
	close(s.q)
	<-s.done
	return s.Conn.(*net.TCPConn).CloseWrite()
}

func (s *slowConn) Close() error {
	select {
	case <-s.done:
	default:
		s.CloseWrite()
	}
	return s.Conn.Close()
}

type slowListener struct{ net.Listener }

func (l slowListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newSlowConn(c), nil
}

var paths = []string{"direct", "native", "vm-tcp", "vm-vsock"}

func BenchmarkIngressConnect(b *testing.B) {
	for _, kind := range append(paths, "vm-slow") {
		b.Run(kind, func(b *testing.B) {
			addr := ingressPath(b, kind)
			buf := make([]byte, 5)
			for b.Loop() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					b.Fatal(err)
				}
				c.Write([]byte("ping\n"))
				if _, err := io.ReadFull(c, buf); err != nil {
					b.Fatal(err)
				}
				c.Close()
			}
		})
	}
}

// BenchmarkIngressAppear measures how long a newly routed port takes to
// serve requests on the host.
func BenchmarkIngressAppear(b *testing.B) {
	for _, kind := range []string{"vm-tcp", "vm-vsock"} {
		b.Run(kind, func(b *testing.B) {
			s := guest(b, kind)
			app := map[string]int{"http": appServer(b, "a")}
			for b.Loop() {
				pub := freePort(b)
				if _, err := s.Route("svc", "svc-a", app, publicPorts(pub)); err != nil {
					b.Fatal(err)
				}
				waitServing(b, fmt.Sprintf("127.0.0.2:%d", pub))
				s.Unroute("svc", "svc-a")
			}
		})
	}
}

const transferSize = 64 << 20

func BenchmarkIngressDownload(b *testing.B) {
	for _, kind := range paths {
		b.Run(kind, func(b *testing.B) {
			addr := ingressPath(b, kind)
			b.SetBytes(transferSize)
			for b.Loop() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					b.Fatal(err)
				}
				fmt.Fprintf(c, "get %d\n", transferSize)
				if n, err := io.CopyN(io.Discard, c, transferSize); err != nil {
					b.Fatalf("got %d bytes: %v", n, err)
				}
				c.Close()
			}
		})
	}
}

func BenchmarkIngressUpload(b *testing.B) {
	for _, kind := range paths {
		b.Run(kind, func(b *testing.B) {
			addr := ingressPath(b, kind)
			b.SetBytes(transferSize)
			chunk := make([]byte, 1<<20)
			buf := make([]byte, 3)
			for b.Loop() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					b.Fatal(err)
				}
				fmt.Fprintf(c, "put %d\n", transferSize)
				for sent := 0; sent < transferSize; sent += len(chunk) {
					if _, err := c.Write(chunk); err != nil {
						b.Fatal(err)
					}
				}
				if _, err := io.ReadFull(c, buf); err != nil {
					b.Fatal(err)
				}
				c.Close()
			}
		})
	}
}
