//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mdlayher/vsock"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// ListenSpec parses "vsock:PORT", "unix:PATH" or "tcp:ADDR".
func ListenSpec(spec string, socketUID, socketGID int) (net.Listener, error) {
	kind, addr, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("invalid listen address %q", spec)
	}
	switch kind {
	case "vsock":
		port, err := strconv.ParseUint(addr, 10, 32)
		if err != nil {
			return nil, err
		}
		l, err := vsock.Listen(uint32(port), nil)
		if err != nil {
			return nil, err
		}
		return proto.TunedVsockListener{Listener: l}, nil
	case "unix":
		if err := os.MkdirAll(filepath.Dir(addr), 0o755); err != nil {
			return nil, err
		}
		os.Remove(addr)
		l, err := net.Listen("unix", addr)
		if err != nil {
			return nil, err
		}
		if socketUID >= 0 {
			if err := os.Chown(addr, socketUID, socketGID); err != nil {
				l.Close()
				return nil, err
			}
			os.Chmod(addr, 0o660)
		}
		return l, nil
	case "tcp":
		return net.Listen("tcp", addr)
	}
	return nil, fmt.Errorf("unsupported listen scheme %q", kind)
}

// Serve runs the agent until ctx is cancelled.
func Serve(ctx context.Context, ls []net.Listener, cfg Config) error {
	s, err := New(cfg)
	if err != nil {
		return err
	}
	s.reconcileIngress(ctx)
	if cfg.Mode == "vm" {
		markBoot("agentReady")
	}
	errc := make(chan error, len(ls))
	for _, l := range ls {
		fmt.Fprintf(os.Stderr, "oar-agent %s listening on %s (mode=%s root=%s)\n", Version, l.Addr(), cfg.Mode, cfg.Root)
		go func(l net.Listener) { errc <- proto.Listen(ctx, l, s.Handle) }(l)
	}
	return <-errc
}
