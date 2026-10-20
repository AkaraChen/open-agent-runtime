package vm

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// ingressWait is how long one ingress.list long poll may wait in the agent.
const ingressWait = 25 * time.Second

// ForwardIngress mirrors the guest's ingress ports on host (an IP, normally
// 127.0.0.1): each host connection is tunnelled through the control channel
// (dial op) to the same port inside the guest. It long-polls the agent's
// ingress list, so a new port is reachable as soon as it is routed.
func ForwardIngress(ctx context.Context, guest *proto.Client, host string, log io.Writer) {
	listeners := map[int]net.Listener{}
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	var ver uint64
	for {
		var list proto.IngressList
		cctx, cancel := context.WithTimeout(ctx, ingressWait+5*time.Second)
		err := guest.Call(cctx, proto.OpIngress, proto.IngressListArgs{Version: ver, Wait: ingressWait}, &list)
		cancel()
		if err == nil {
			ver = list.Version
			want := map[int]bool{}
			for _, ig := range list.Ingresses {
				want[ig.Port] = true
				if _, ok := listeners[ig.Port]; ok {
					continue
				}
				l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(ig.Port)))
				if err != nil {
					fmt.Fprintf(log, "forward port %d: %v\n", ig.Port, err)
					continue
				}
				fmt.Fprintf(log, "forwarding %s:%d -> guest :%d (%s/%s)\n", host, ig.Port, ig.Port, ig.Container, ig.Name)
				listeners[ig.Port] = l
				go acceptForward(ctx, l, guest, ig.Port)
			}
			for port, l := range listeners {
				if !want[port] {
					l.Close()
					delete(listeners, port)
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

func acceptForward(ctx context.Context, l net.Listener, guest *proto.Client, port int) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			g, err := guest.Stream(ctx, proto.OpDial, proto.DialArgs{Port: port})
			if err != nil {
				return
			}
			proto.Pipe(c, g)
		}()
	}
}
