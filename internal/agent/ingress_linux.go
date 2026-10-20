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
			pipe(c, b)
		}()
	}
}

// pipe copies both directions and closes both connections.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

func (s *Server) ingressList() proto.IngressList {
	ig := &s.ing
	ig.mu.Lock()
	defer ig.mu.Unlock()
	res := proto.IngressList{Ingresses: []proto.Ingress{}}
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
	return res
}

// handleDial connects the request stream to a TCP port inside the guest
// network (host-side port forwarding).
func (s *Server) handleDial(a proto.DialArgs, rs *proto.ResponseStream) error {
	if a.Port < 1 || a.Port > 65535 {
		return fmt.Errorf("invalid port %d", a.Port)
	}
	b, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", a.Port), 2*time.Second)
	if err != nil {
		return err
	}
	conn, r, err := rs.Hijack(nil)
	if err != nil {
		b.Close()
		return err
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(b, r)
		if cw, ok := b.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, b)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	b.Close()
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
