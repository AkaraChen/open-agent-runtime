//go:build linux

package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sync"

	"github.com/AkaraChen/open-agent-runtime/internal/agent"
	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// native runs the guest agent directly on a Linux host: no VM, the host
// kernel is the "guest". It is the fastest backend for Linux development.
type native struct{}

func init() { Register(native{}) }

func (native) Name() string          { return "native" }
func (native) NeedsGuestImage() bool { return false }
func (native) NeedsRoot() bool       { return true }

func (native) Available() error {
	if _, err := exec.LookPath(runcBinary()); err != nil {
		return fmt.Errorf("runc not found in PATH (install runc or set OAR_RUNC)")
	}
	return nil
}

func runcBinary() string {
	if r := os.Getenv("OAR_RUNC"); r != "" {
		return r
	}
	return "runc"
}

type nativeMachine struct {
	srv  *agent.Server
	ctx  context.Context
	done chan struct{}
	once sync.Once
}

func (native) Boot(ctx context.Context, cfg MachineConfig) (Machine, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("the native backend must run as root")
	}
	if cfg.Arch != "" && cfg.Arch != runtime.GOARCH {
		return nil, fmt.Errorf("native backend can only run %s containers on this host", runtime.GOARCH)
	}
	root := cfg.Root
	if root == "" {
		root = agent.GuestRoot
	}
	srv, err := agent.New(agent.Config{
		Root:      root,
		Mode:      "native",
		StoreView: cfg.StoreView,
		Runc:      agent.Runc{Binary: runcBinary(), Root: "/run/oar/runc"},
	})
	if err != nil {
		return nil, err
	}
	m := &nativeMachine{srv: srv, ctx: context.WithoutCancel(ctx), done: make(chan struct{})}
	srv.SetShutdown(func() {
		srv.StopAll(m.ctx)
		m.once.Do(func() { close(m.done) })
	})
	return m, nil
}

// Transport reports the control channel.
func (m *nativeMachine) Transport() string { return "inprocess" }

func (m *nativeMachine) DialAgent(ctx context.Context) (net.Conn, error) {
	select {
	case <-m.done:
		return nil, fmt.Errorf("agent stopped")
	default:
	}
	c1, c2 := net.Pipe()
	go proto.Serve(m.ctx, c2, m.srv.Handle)
	return c1, nil
}

func (m *nativeMachine) Done() <-chan struct{} { return m.done }

func (m *nativeMachine) Kill() error {
	m.once.Do(func() { close(m.done) })
	return nil
}
