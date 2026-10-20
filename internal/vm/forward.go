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

// forwardIngress mirrors the guest's ingress ports on the host loopback:
// each host connection is tunnelled through the control channel (dial op)
// to the same port inside the guest.
func forwardIngress(ctx context.Context, guest *proto.Client, log io.Writer) {
	var mu sync.Mutex
	listeners := map[int]net.Listener{}
	defer func() {
		mu.Lock()
		for _, l := range listeners {
			l.Close()
		}
		mu.Unlock()
	}()
	for {
		var list proto.IngressList
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := guest.Call(cctx, proto.OpIngress, nil, &list)
		cancel()
		if err == nil {
			want := map[int]bool{}
			for _, ig := range list.Ingresses {
				want[ig.Port] = true
				mu.Lock()
				_, ok := listeners[ig.Port]
				mu.Unlock()
				if ok {
					continue
				}
				l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ig.Port))
				if err != nil {
					fmt.Fprintf(log, "forward port %d: %v\n", ig.Port, err)
					continue
				}
				fmt.Fprintf(log, "forwarding 127.0.0.1:%d -> guest :%d (%s/%s)\n", ig.Port, ig.Port, ig.Container, ig.Name)
				mu.Lock()
				listeners[ig.Port] = l
				mu.Unlock()
				go acceptForward(ctx, l, guest, ig.Port)
			}
			mu.Lock()
			for port, l := range listeners {
				if !want[port] {
					l.Close()
					delete(listeners, port)
				}
			}
			mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
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
			defer g.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(g, c); closeWrite(g); done <- struct{}{} }()
			go func() { io.Copy(c, g); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}
