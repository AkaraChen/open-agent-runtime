//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
)

// appendTo adds to a log; it may run on another goroutine than the test's.
func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Error(err)
	}
}

func TestLogSearch(t *testing.T) {
	const needle = "my-agent: serving on 127.0.0.1:18080"
	t.Run("grows", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "console.log")
		l := logSearch{path: log, needle: []byte(needle)}
		defer l.close()
		if l.found() {
			t.Fatal("found the string in a log that does not exist")
		}
		appendTo(t, log, "starting\n")
		if l.found() {
			t.Fatal("found the string in a log without it")
		}
		appendTo(t, log, "my-agent: serving on 127.0.0.1:1808") // not complete yet
		if l.found() {
			t.Fatal("found an incomplete string")
		}
		appendTo(t, log, "0\n")
		if !l.found() {
			t.Fatal("did not find the string that was completed by the last write")
		}
		if st, _ := os.Stat(log); l.off != st.Size() {
			t.Fatalf("read %d of %d bytes", l.off, st.Size())
		}
	})
	t.Run("split everywhere", func(t *testing.T) {
		for split := 1; split < len(needle); split++ {
			log := filepath.Join(t.TempDir(), "console.log")
			l := logSearch{path: log, needle: []byte(needle)}
			appendTo(t, log, "noise\n"+needle[:split])
			if l.found() {
				t.Fatalf("split %d: found the first part only", split)
			}
			appendTo(t, log, needle[split:]+"\n")
			if !l.found() {
				t.Fatalf("split %d: missed a string that straddles two reads", split)
			}
			l.close()
		}
	})
	t.Run("across the buffer", func(t *testing.T) {
		// the string starts a few bytes before, at, and after the end of
		// the first buffer-full
		for shift := -len(needle) - 1; shift <= 2; shift++ {
			log := filepath.Join(t.TempDir(), "console.log")
			l := logSearch{path: log, needle: []byte(needle)}
			appendTo(t, log, strings.Repeat("x", 32<<10+shift)+needle+strings.Repeat("y", 70<<10))
			if !l.found() {
				t.Fatalf("shift %d: string not found in a log of several buffers", shift)
			}
			l.close()
		}
		log := filepath.Join(t.TempDir(), "console.log")
		l := logSearch{path: log, needle: []byte(needle)}
		defer l.close()
		appendTo(t, log, strings.Repeat("no\n", 100<<10))
		if l.found() {
			t.Fatal("found a string that is not in the log")
		}
		if st, _ := os.Stat(log); l.off != st.Size() {
			t.Fatalf("read %d of %d bytes", l.off, st.Size())
		}
	})
	t.Run("one byte", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "console.log")
		l := logSearch{path: log, needle: []byte("!")}
		defer l.close()
		appendTo(t, log, "abc")
		if l.found() {
			t.Fatal("found !")
		}
		appendTo(t, log, "!")
		if !l.found() {
			t.Fatal("missed !")
		}
	})
}

func TestPollPause(t *testing.T) {
	ms := time.Millisecond
	for waited, want := range map[time.Duration]time.Duration{
		0:         ms,
		ms:        ms,
		49 * ms:   ms,
		50 * ms:   ms,
		99 * ms:   1980 * time.Microsecond,
		100 * ms:  2 * ms,
		250 * ms:  5 * ms,
		499 * ms:  9980 * time.Microsecond,
		500 * ms:  10 * ms,
		time.Hour: 10 * ms,
	} {
		if got := pollPause(waited); got != want {
			t.Errorf("after %s: pause %s, want %s", waited, got, want)
		}
	}
	// a probe is never late by more than 2% of the wait (beyond the first
	// millisecond) or by more than the 10 ms of the old loop
	for waited := time.Duration(0); waited < 2*time.Second; waited += 137 * time.Microsecond {
		p := pollPause(waited)
		if p < ms || p > 10*ms || p > max(ms, waited/50) {
			t.Fatalf("after %s: pause %s", waited, p)
		}
	}
}

// readyServer is a server whose runc is a fake that records its calls, with
// a real process as the init of container "web-1".
func readyServer(t *testing.T) (s *Server, in *instance, child interface{ Kill() error }, calls string) {
	t.Helper()
	root := t.TempDir()
	bin, calls := fakeRunc(t)
	proc := startProcess(t, "sleep")
	_, start, err := procStat(proc.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	writeState(t, root, "web-1", proc.Process.Pid, start)
	dir := filepath.Join(t.TempDir(), "web-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: Config{Runc: Runc{Binary: bin, Root: root}}},
		&instance{Name: "web", ID: "web-1", dir: dir}, proc.Process, calls
}

func TestWaitReadyTCP(t *testing.T) {
	s, in, _, calls := readyServer(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
		}
		listening <- l
	}()
	t0 := time.Now()
	err := s.waitReady(context.Background(), in, &manifest.Ready{TCP: addr, Timeout: 10 * time.Second})
	if l := <-listening; l != nil {
		l.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d < 40*time.Millisecond {
		t.Fatalf("ready after %s, before the listener was there", d)
	}
	if got := execs(t, calls); len(got) != 0 {
		t.Fatalf("runc was run while waiting for readiness: %q", got)
	}
}

func TestWaitReadyLog(t *testing.T) {
	s, in, _, calls := readyServer(t)
	log := filepath.Join(in.dir, "console.log")
	go func() {
		time.Sleep(20 * time.Millisecond)
		appendTo(t, log, "booting\nmy-agent: serv")
		time.Sleep(20 * time.Millisecond)
		appendTo(t, log, "ing on :8080\n")
	}()
	if err := s.waitReady(context.Background(), in, &manifest.Ready{Log: "serving on :8080", Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if got := execs(t, calls); len(got) != 0 {
		t.Fatalf("runc was run while waiting for readiness: %q", got)
	}
}

func TestWaitReadyExited(t *testing.T) {
	s, in, child, calls := readyServer(t)
	go func() { time.Sleep(30 * time.Millisecond); child.Kill() }()
	t0 := time.Now()
	err := s.waitReady(context.Background(), in, &manifest.Ready{TCP: fmt.Sprintf("127.0.0.1:%d", freePort(t)), Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("the exit was noticed after %s", d)
	}
	if got := execs(t, calls); len(got) != 0 {
		t.Fatalf("runc was run while waiting for readiness: %q", got)
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	s, in, _, calls := readyServer(t)
	t0 := time.Now()
	err := s.waitReady(context.Background(), in, &manifest.Ready{Log: "never printed", Timeout: 100 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "readiness timed out after 100ms") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(t0); d < 100*time.Millisecond {
		t.Fatalf("timed out after %s", d)
	}
	if got := execs(t, calls); len(got) != 0 {
		t.Fatalf("runc was run while waiting for readiness: %q", got)
	}
}

func TestWaitReadyCancelled(t *testing.T) {
	s, in, _, _ := readyServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.waitReady(ctx, in, &manifest.Ready{Log: "never printed", Timeout: time.Minute}); err != context.DeadlineExceeded {
		t.Fatalf("got %v", err)
	}
}

// The exec probe runs a process in the container, so it is the one probe
// that runs runc, and the one that keeps its 10 ms between rounds.
func TestWaitReadyExec(t *testing.T) {
	s, in, _, calls := readyServer(t)
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\n[ \"$3\" = exec ] || exit 0\n[ \"$(grep -c ' exec ' %s)\" -ge 3 ]\n", calls, calls)
	if err := os.WriteFile(s.cfg.Runc.Binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := s.waitReady(context.Background(), in, &manifest.Ready{Exec: []string{"true"}, Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d < 20*time.Millisecond {
		t.Fatalf("three probes took %s; the pause between them is 10ms", d)
	}
	got := execs(t, calls)
	if len(got) != 3 {
		t.Fatalf("runc calls: %q", got)
	}
	for _, c := range got {
		if !strings.Contains(c, " exec web-1 true") {
			t.Fatalf("runc was run for something else than the probe: %q", got)
		}
	}
}
