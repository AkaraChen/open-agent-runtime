package vm

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// ingressWait is how long one ingress.list long poll may wait in the agent.
const ingressWait = 25 * time.Second

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

// ForwardIngress mirrors the guest's ingress ports on host (an IP, normally
// 127.0.0.1), tunnelling each connection through the control channel.
func ForwardIngress(ctx context.Context, guest *proto.Client, host string, log io.Writer) {
	forwardIngress(ctx, guest, nil, host, log)
}

// forwardIngress mirrors the guest's ingress ports on host. Depending on
// the machine a port is forwarded by the VMM, proxied to the guest's
// address, or tunnelled through the control channel (dial op). It
// long-polls the agent's ingress list, so a new port is reachable as soon
// as it is routed.
func forwardIngress(ctx context.Context, guest *proto.Client, m Machine, host string, log io.Writer) {
	listeners := map[int]net.Listener{}
	vmm := map[int]bool{} // ports forwarded by the VMM
	var mu sync.Mutex
	var guestIP string // guarded by mu
	hf, _ := m.(hostForwarder)
	gn, _ := m.(guestNetwork)
	direct := gn != nil && gn.GuestNetwork()
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
		for port := range vmm {
			hf.HostUnforward(port)
		}
	}()
	addr := func() string { mu.Lock(); defer mu.Unlock(); return guestIP }
	var ver uint64
	for {
		var list proto.IngressList
		cctx, cancel := context.WithTimeout(ctx, ingressWait+5*time.Second)
		err := guest.Call(cctx, proto.OpIngress, proto.IngressListArgs{Version: ver, Wait: ingressWait}, &list)
		cancel()
		if err == nil {
			ver = list.Version
			mu.Lock()
			guestIP = list.GuestIP
			mu.Unlock()
			want := map[int]bool{}
			for _, ig := range list.Ingresses {
				want[ig.Port] = true
				if _, ok := listeners[ig.Port]; ok || vmm[ig.Port] {
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
				l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(ig.Port)))
				if err != nil {
					fmt.Fprintf(log, "forward port %d: %v\n", ig.Port, err)
					continue
				}
				via := "control channel"
				var dialGuest func() string
				if direct {
					via, dialGuest = "guest network, control channel as fallback", addr
				}
				fmt.Fprintf(log, "forwarding %s:%d -> guest :%d (%s/%s) via %s\n", host, ig.Port, ig.Port, ig.Container, ig.Name, via)
				listeners[ig.Port] = l
				go acceptForward(ctx, l, guest, ig.Port, dialGuest)
			}
			for port, l := range listeners {
				if !want[port] {
					l.Close()
					delete(listeners, port)
				}
			}
			for port := range vmm {
				if !want[port] {
					hf.HostUnforward(port)
					delete(vmm, port)
				}
			}
		}
		// errors, and agents without long polling (version 0), answer
		// at once: poll them at a fixed interval
		wait := time.Duration(0)
		if err != nil || list.Version == 0 {
			wait = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
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
					g, _ = net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), time.Second)
				}
			}
			if g == nil {
				var err error
				if g, err = guest.Stream(ctx, proto.OpDial, proto.DialArgs{Port: port}); err != nil {
					return
				}
			}
			proto.Pipe(c, g)
		}()
	}
}
