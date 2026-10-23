//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

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
	handle := s.Handle
	if cfg.Mode == "vm" && cmdlineParam("oar.dumpkmsg") == "1" {
		handle = afterFirstRequest(handle, func() { dumpKmsg("/dev/kmsg", os.Stderr) })
	}
	errc := make(chan error, len(ls))
	for _, l := range ls {
		fmt.Fprintf(os.Stderr, "oar-agent %s listening on %s (mode=%s root=%s)\n", Version, l.Addr(), cfg.Mode, cfg.Root)
		go func(l net.Listener) { errc <- proto.Listen(ctx, l, handle) }(l)
	}
	if addr := cmdlineParam("oar.notify"); addr != "" && cfg.Mode == "vm" {
		go notifyReady(addr)
	}
	return <-errc
}

// notifyReady tells the host that the agent accepts connections, so it
// can connect at once instead of waiting out a probe that was started
// before the agent listened (the host only needs the connection itself).
func notifyReady(addr string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// afterFirstRequest wraps h to start f, in a goroutine, once the first
// request, the host's readiness probe, has been handled: for what must not
// get in the way of the boot the host measures up to its first answer.
func afterFirstRequest(h proto.Handler, f func()) proto.Handler {
	var once sync.Once
	return func(ctx context.Context, req *proto.Request, body io.Reader, rs *proto.ResponseStream) (any, error) {
		defer once.Do(func() { go f() })
		return h(ctx, req, body, rs)
	}
}

// dumpKmsg writes the kernel log from path (/dev/kmsg) to w and ends with a
// line saying "kernel log dump done" or "failed". It is for profiling the
// boot (oar.dumpkmsg=1 on the kernel command line, off by default): the
// boot-profile workflow reads the dump from the console log and waits for
// that line.
//
// PERF: this keeps initcall_debug from changing the boot it measures. With a
// console loglevel that shows the initcall messages the kernel sends every
// one of them through the emulated serial port, a VM exit per character, and
// a 0.4 s boot takes 2-4 s. The messages are in the kernel's ring buffer at
// any loglevel (log_buf_len=4M is large enough for all of them), so the agent
// writes the buffer out when the boot is over: after the host's first answer
// (afterFirstRequest), not when the agent starts to listen, because the dump
// loads the console and a vCPU (on QEMU/TCG it delayed that answer by about
// 0.6 s). The price is that load afterwards.
func dumpKmsg(path string, w io.Writer) {
	// non-blocking: /dev/kmsg returns EAGAIN, instead of waiting for the
	// next message, once the buffer is drained
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(w, "oar-agent: kernel log dump failed: %v\n", err)
		return
	}
	defer unix.Close(fd)
	// every read returns one record of at most 8 KiB (a smaller buffer
	// fails with EINVAL)
	buf := make([]byte, 16<<10)
	records := 0
	for {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EPIPE) {
			continue // EPIPE: records were overwritten; the next read goes on with the oldest left
		}
		if err != nil || n == 0 {
			break
		}
		w.Write(buf[:n])
		records++
	}
	fmt.Fprintf(w, "oar-agent: kernel log dump done (%d records)\n", records)
}
