//go:build linux

package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

const kmsgSample = "6,0,1000,-;Linux version 7.0.0-oar\n" +
	"7,1,2000,-;calling  jent_mod_init+0x0/0x80 @ 1\n" +
	"7,2,9000,-;initcall jent_mod_init+0x0/0x80 returned 0 after 7000 usecs\n"

// dump runs dumpKmsg on path and fails the test if it does not return.
func dump(t *testing.T, path string) string {
	t.Helper()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		dumpKmsg(path, &out)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("dumpKmsg did not return")
	}
	return out.String()
}

func TestDumpKmsgFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kmsg")
	if err := os.WriteFile(path, []byte(kmsgSample), 0o644); err != nil {
		t.Fatal(err)
	}
	got := dump(t, path)
	if !strings.HasPrefix(got, kmsgSample) || !strings.HasSuffix(got, "oar-agent: kernel log dump done (1 records)\n") {
		t.Errorf("dump:\n%s", got)
	}
}

// TestDumpKmsgStopsWhenDrained uses a pipe with its writer still open, which
// like /dev/kmsg answers a non-blocking read with EAGAIN once it is empty
// (a blocking read would wait for the next message).
func TestDumpKmsgStopsWhenDrained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kmsg")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skip("no fifo:", err)
	}
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.WriteString(kmsgSample); err != nil {
		t.Fatal(err)
	}
	got := dump(t, path)
	if !strings.HasPrefix(got, kmsgSample) || !strings.Contains(got, "kernel log dump done") {
		t.Errorf("dump:\n%s", got)
	}
}

func TestDumpKmsgMissing(t *testing.T) {
	got := dump(t, filepath.Join(t.TempDir(), "none"))
	if !strings.Contains(got, "kernel log dump failed") || strings.Contains(got, "dump done") {
		t.Errorf("dump:\n%s", got)
	}
}

func TestAfterFirstRequest(t *testing.T) {
	started := make(chan struct{}, 4)
	h := func(ctx context.Context, req *proto.Request, body io.Reader, rs *proto.ResponseStream) (any, error) {
		select {
		case <-started:
			t.Errorf("%s: the function ran before the request was handled", req.Op)
		default:
		}
		return "result of " + req.Op, nil
	}
	w := afterFirstRequest(h, func() { started <- struct{}{} })

	select {
	case <-started:
		t.Fatal("the function ran without a request")
	case <-time.After(20 * time.Millisecond):
	}
	res, err := w(context.Background(), &proto.Request{Op: "info"}, nil, nil)
	if res != "result of info" || err != nil {
		t.Errorf("the handler's answer was changed: %v, %v", res, err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the function did not run after the first request")
	}
	// once only
	for _, op := range []string{"ps", "stats", "info"} {
		if res, _ := w(context.Background(), &proto.Request{Op: op}, nil, nil); res != "result of "+op {
			t.Errorf("%s: %v", op, res)
		}
	}
	select {
	case <-started:
		t.Error("the function ran again")
	case <-time.After(50 * time.Millisecond):
	}
}
