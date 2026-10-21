//go:build linux

package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// The ingress proxy owns a container's public ports. Every instance listens
// on its own private port; switching instances atomically repoints the
// public port, so new connections go to the new instance while the old one
// finishes its in-flight connections (no SO_REUSEPORT, no port conflicts,
// and a fully started standby can wait for traffic).

type ingressTarget struct {
	addr     string
	instance string
	active   atomic.Int64
}

type ingress struct {
	container string
	name      string
	port      int
	ln        net.Listener
	target    atomic.Pointer[ingressTarget]
}

type ingresses struct {
	mu sync.Mutex
	m  map[string]map[string]*ingress // container -> port name
	// ver changes with every route change; changed is closed then
	// (long-polling ingress.list waits on it)
	ver     uint64
	changed chan struct{}
}

// bumpLocked records a change and wakes waiting ingress.list calls.
func (ig *ingresses) bumpLocked() {
	ig.ver++
	if ig.changed != nil {
		close(ig.changed)
		ig.changed = nil
	}
}

func (s *Server) ingressAddr() string {
	if s.cfg.Mode == "native" {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

// allocatePorts picks free private ports for an instance.
func allocatePorts(ports []manifest.Port) (map[string]int, error) {
	out := map[string]int{}
	for _, p := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		out[p.Name] = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	return out, nil
}

// route points the container's public ports at instance in and returns the
// previous targets (to be drained).
func (s *Server) route(in *instance) ([]*ingressTarget, error) {
	ig := &s.ing
	ig.mu.Lock()
	defer ig.mu.Unlock()
	defer ig.bumpLocked()
	if ig.m == nil {
		ig.m = map[string]map[string]*ingress{}
	}
	cur := ig.m[in.Name]
	if cur == nil {
		cur = map[string]*ingress{}
		ig.m[in.Name] = cur
	}
	var olds []*ingressTarget
	want := map[string]bool{}
	for _, p := range in.PublicPorts {
		want[p.Name] = true
		g := cur[p.Name]
		if g != nil && g.port != p.Port {
			g.ln.Close()
			if t := g.target.Load(); t != nil {
				olds = append(olds, t)
			}
			g = nil
		}
		if g == nil {
			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.ingressAddr(), p.Port))
			if err != nil {
				return olds, fmt.Errorf("ingress %s on port %d: %w", p.Name, p.Port, err)
			}
			g = &ingress{container: in.Name, name: p.Name, port: p.Port, ln: ln}
			cur[p.Name] = g
			go g.serve()
		}
		t := &ingressTarget{addr: fmt.Sprintf("127.0.0.1:%d", in.Ports[p.Name]), instance: in.ID}
		if old := g.target.Swap(t); old != nil {
			olds = append(olds, old)
		}
	}
	for name, g := range cur {
		if !want[name] {
			g.ln.Close()
			if t := g.target.Load(); t != nil {
				olds = append(olds, t)
			}
			delete(cur, name)
		}
	}
	return olds, nil
}

// unroute closes the container's public ports if they point at instance id.
func (s *Server) unroute(container, id string) {
	ig := &s.ing
	ig.mu.Lock()
	defer ig.mu.Unlock()
	defer ig.bumpLocked()
	for name, g := range ig.m[container] {
		if t := g.target.Load(); t == nil || t.instance == id {
			g.ln.Close()
			delete(ig.m[container], name)
		}
	}
}

func (s *Server) closeAllIngress() {
	ig := &s.ing
	ig.mu.Lock()
	defer ig.mu.Unlock()
	defer ig.bumpLocked()
	for _, ports := range ig.m {
		for _, g := range ports {
			g.ln.Close()
		}
	}
	ig.m = nil
}

// drain waits until no connections are in flight on the targets.
func drain(ctx context.Context, targets []*ingressTarget, timeout time.Duration) time.Duration {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		busy := false
		for _, t := range targets {
			if t.active.Load() > 0 {
				busy = true
			}
		}
		if !busy || time.Now().After(deadline) || ctx.Err() != nil {
			return time.Since(start)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (g *ingress) serve() {
	for {
		c, err := g.ln.Accept()
		if err != nil {
			return
		}
		t := g.target.Load()
		if t == nil {
			c.Close()
			continue
		}
		t.active.Add(1)
		go func() {
			defer t.active.Add(-1)
			b, err := net.DialTimeout("tcp", t.addr, 2*time.Second)
			if err != nil {
				c.Close()
				return
			}
			proto.Pipe(c, b)
		}()
	}
}

func (s *Server) ingressList() proto.IngressList {
	ig := &s.ing
	ig.mu.Lock()
	defer ig.mu.Unlock()
	res := proto.IngressList{Version: ig.ver, Ingresses: []proto.Ingress{}}
	for container, ports := range ig.m {
		for name, g := range ports {
			e := proto.Ingress{Container: container, Name: name, Port: g.port}
			if t := g.target.Load(); t != nil {
				e.Target, e.Instance, e.Active = t.addr, t.instance, t.active.Load()
			}
			res.Ingresses = append(res.Ingresses, e)
		}
	}
	sort.Slice(res.Ingresses, func(i, j int) bool { return res.Ingresses[i].Port < res.Ingresses[j].Port })
	if s.cfg.Mode == "vm" {
		res.GuestIP = interfaceIPv4("eth0")
	}
	return res
}

func interfaceIPv4(name string) string {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

// ingressWait is ingressList as a long poll (see proto.IngressListArgs).
func (s *Server) ingressWait(ctx context.Context, a proto.IngressListArgs) proto.IngressList {
	ig := &s.ing
	ig.mu.Lock()
	if a.Wait <= 0 || ig.ver != a.Version {
		ig.mu.Unlock()
		return s.ingressList()
	}
	if ig.changed == nil {
		ig.changed = make(chan struct{})
	}
	changed := ig.changed
	ig.mu.Unlock()
	t := time.NewTimer(min(a.Wait, time.Minute))
	defer t.Stop()
	select {
	case <-changed:
	case <-t.C:
	case <-ctx.Done():
	}
	return s.ingressList()
}

// ingressTarget returns the current target of the ingress on port, if any.
func (s *Server) ingressTarget(port int) *ingressTarget {
	ig := &s.ing
	ig.mu.Lock()
	defer ig.mu.Unlock()
	for _, ports := range ig.m {
		for _, g := range ports {
			if g.port == port {
				return g.target.Load()
			}
		}
	}
	return nil
}

// handleDial connects the request stream to a TCP port inside the guest
// network (host-side port forwarding). An ingress port is dialed at its
// current instance directly instead of through the ingress listener, which
// saves a hop; the connection still counts for draining.
func (s *Server) handleDial(a proto.DialArgs, rs *proto.ResponseStream) error {
	if a.Port < 1 || a.Port > 65535 {
		return fmt.Errorf("invalid port %d", a.Port)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", a.Port)
	if t := s.ingressTarget(a.Port); t != nil {
		t.active.Add(1)
		defer t.active.Add(-1)
		addr = t.addr
	}
	b, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	conn, r, err := rs.Hijack(nil)
	if err != nil {
		b.Close()
		return err
	}
	proto.Pipe(&readerConn{conn, r}, b)
	return nil
}

// readerConn reads through r, which holds bytes already buffered from Conn.
type readerConn struct {
	net.Conn
	r io.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *readerConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// reconcileIngress restores public ports for running instances after an
// agent restart.
func (s *Server) reconcileIngress(ctx context.Context) {
	list, err := s.loadInstances()
	if err != nil {
		return
	}
	for _, in := range list {
		if in.Retired || in.Prepared || len(in.PublicPorts) == 0 {
			continue
		}
		if st := s.observe(ctx, in); st.Status == "running" {
			s.route(in)
		}
	}
}
