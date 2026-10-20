package vm

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// hostForwarder is a machine whose VMM can forward a host loopback port to
// the same guest port itself (QEMU user networking): connections then skip
// the supervisor, the control channel and the agent.
type hostForwarder interface {
	HostForward(port int) error
	HostUnforward(port int)
}

// guestNetwork is a machine whose guest address is reachable from the host
// (VZ NAT): the supervisor connects to it directly instead of through the
// control channel.
type guestNetwork interface {
	GuestNetwork() bool
}

// forwardIngress mirrors the guest's ingress ports on the host loopback.
// Depending on the backend a port is forwarded by the VMM, proxied to the
// guest's address, or tunnelled through the control channel (dial op).
func forwardIngress(ctx context.Context, guest *proto.Client, m Machine, log io.Writer) {
	var mu sync.Mutex
	listeners := map[int]net.Listener{}
	vmm := map[int]bool{} // ports forwarded by the VMM
	var guestIP string    // guarded by mu
	hf, _ := m.(hostForwarder)
	gn, _ := m.(guestNetwork)
	direct := gn != nil && gn.GuestNetwork()
	defer func() {
		mu.Lock()
		for _, l := range listeners {
			l.Close()
		}
		mu.Unlock()
		for port := range vmm {
			hf.HostUnforward(port)
		}
	}()
	addr := func() string { mu.Lock(); defer mu.Unlock(); return guestIP }
	for {
		var list proto.IngressList
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := guest.Call(cctx, proto.OpIngress, nil, &list)
		cancel()
		if err == nil {
			mu.Lock()
			guestIP = list.GuestIP
			mu.Unlock()
			want := map[int]bool{}
			for _, ig := range list.Ingresses {
				want[ig.Port] = true
				mu.Lock()
				_, ok := listeners[ig.Port]
				mu.Unlock()
				if ok || vmm[ig.Port] {
					continue
				}
				if hf != nil {
					if err := hf.HostForward(ig.Port); err == nil {
						fmt.Fprintf(log, "forwarding 127.0.0.1:%d -> guest :%d (%s/%s) by the VMM\n", ig.Port, ig.Port, ig.Container, ig.Name)
						vmm[ig.Port] = true
						continue
					} else {
						fmt.Fprintf(log, "VMM forward of port %d: %v; using the control channel\n", ig.Port, err)
					}
				}
				l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ig.Port))
				if err != nil {
					fmt.Fprintf(log, "forward port %d: %v\n", ig.Port, err)
					continue
				}
				via := "control channel"
				if direct {
					via = "guest network, control channel as fallback"
				}
				fmt.Fprintf(log, "forwarding 127.0.0.1:%d -> guest :%d (%s/%s) via %s\n", ig.Port, ig.Port, ig.Container, ig.Name, via)
				mu.Lock()
				listeners[ig.Port] = l
				mu.Unlock()
				var dialGuest func() string
				if direct {
					dialGuest = addr
				}
				go acceptForward(ctx, l, guest, ig.Port, dialGuest)
			}
			mu.Lock()
			for port, l := range listeners {
				if !want[port] {
					l.Close()
					delete(listeners, port)
				}
			}
			mu.Unlock()
			for port := range vmm {
				if !want[port] {
					hf.HostUnforward(port)
					delete(vmm, port)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// acceptForward proxies host connections to the guest port: directly to
// the guest's address when guestAddr reports one, otherwise (or if that
// fails) through the control channel.
func acceptForward(ctx context.Context, l net.Listener, guest *proto.Client, port int, guestAddr func() string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var g net.Conn
			if guestAddr != nil {
				if ip := guestAddr(); ip != "" {
					g, _ = net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprint(port)), time.Second)
				}
			}
			if g == nil {
				var err error
				if g, err = guest.Stream(ctx, proto.OpDial, proto.DialArgs{Port: port}); err != nil {
					return
				}
			}
			defer g.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(g, c); closeWrite(g); done <- struct{}{} }()
			go func() { io.Copy(c, g); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}
