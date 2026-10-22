//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// info answers while the lookup of runc's version is still running. The
// host's readiness probe is the first info call, and exec'ing runc takes
// 100-200 ms in an emulated guest; the version shows up in a later call.
func TestInfoDoesNotWaitForRunc(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	fake := filepath.Join(dir, "runc")
	// a runc that answers only once the gate file exists
	script := "#!/bin/sh\nwhile [ ! -e " + gate + " ]; do sleep 0.01; done\necho 'runc version fake'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// With the gate open it answers at once. Try it before the server does:
	// a script written a moment ago can fail with ETXTBSY if another
	// goroutine forks in the meantime.
	r := Runc{Binary: fake}
	for i := 0; i < 100 && r.Version() != "runc version fake"; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.Version(); got != "runc version fake" {
		if strings.Contains(got, "permission denied") {
			t.Skipf("cannot run a script from %s: %s", dir, got)
		}
		t.Fatalf("the fake runc answers %q", got)
	}
	os.Remove(gate)

	s, err := New(Config{Root: filepath.Join(dir, "root"), Mode: "native", Runc: Runc{Binary: fake, Root: filepath.Join(dir, "runc-state")}})
	if err != nil {
		t.Fatal(err)
	}
	// the lookup is stuck at the gate now
	got := make(chan proto.InfoResult, 1)
	go func() { got <- s.info() }()
	select {
	case info := <-got:
		if info.Runc != "" {
			t.Fatalf("info reported runc %q while the lookup was blocked", info.Runc)
		}
		if info.Protocol != proto.Version {
			t.Fatalf("info protocol %d, want %d", info.Protocol, proto.Version)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("info waits for the runc version")
	}
	// once the lookup finishes, info reports the version
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for s.info().Runc != "runc version fake" {
		if time.Now().After(deadline) {
			t.Fatalf("info never reported the runc version (last %q)", s.info().Runc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
