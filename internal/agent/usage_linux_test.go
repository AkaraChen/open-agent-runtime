//go:build linux

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

func TestUsageSince(t *testing.T) {
	a := usage{runcExecs: 2, user: 30 * time.Millisecond, sys: 10 * time.Millisecond, minflt: 100, majflt: 1}
	b := usage{runcExecs: 5, user: 80 * time.Millisecond, sys: 25 * time.Millisecond, minflt: 450, majflt: 4}
	d := b.since(a)
	if want := (usage{runcExecs: 3, user: 50 * time.Millisecond, sys: 15 * time.Millisecond, minflt: 350, majflt: 3}); d != want {
		t.Fatalf("since: got %+v, want %+v", d, want)
	}
	if z := b.since(b); z != (usage{}) {
		t.Fatalf("a reading since itself is %+v, want zero", z)
	}
	var tm proto.Timings
	d.addTo(&tm)
	if tm.RuncExecs != 3 || tm.CPUUser != 50*time.Millisecond || tm.CPUSys != 15*time.Millisecond || tm.MinorFaults != 350 || tm.MajorFaults != 3 {
		t.Fatalf("addTo: %+v", tm)
	}
}

// TestReadUsageCountsChildren checks that the CPU time and faults of a
// child that has been waited for show up in the delta, and that a child
// which is not runc does not count as an exec.
func TestReadUsageCountsChildren(t *testing.T) {
	before := readUsage()
	out, err := exec.Command("sh", "-c", `i=0; while [ $i -lt 30000 ]; do i=$((i+1)); done`).CombinedOutput()
	if err != nil {
		t.Fatalf("busy child: %v %s", err, out)
	}
	used := readUsage().since(before)
	if used.user+used.sys <= 0 {
		t.Errorf("no CPU time counted for a busy child: %+v", used)
	}
	if used.minflt <= 0 {
		t.Errorf("no page faults counted for a new process: %+v", used)
	}
	if used.runcExecs != 0 {
		t.Errorf("a child that is not runc counted as %d execs", used.runcExecs)
	}
}

// fakeRunc returns a Runc whose binary is a script: `state` prints the
// contents of state.json next to it, `--version` a version line, and
// everything else succeeds silently.
func fakeRunc(t *testing.T, state string) *Runc {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "runc")
	body := `#!/bin/sh
case "$1" in --version) echo "runc version fake"; exit 0 ;; esac
for a in "$@"; do
  if [ "$a" = state ]; then cat "$(dirname "$0")/state.json"; fi
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(script, "--version").Run(); err != nil {
		t.Skipf("cannot run scripts from %s: %v", dir, err)
	}
	return &Runc{Binary: script, Root: filepath.Join(dir, "root")}
}

// TestRuncExecsCountsEveryCommand runs each way the agent starts runc and
// checks that it counts exactly one exec: a missed path would hide runc
// processes from the benchmarks, a double count would inflate them.
func TestRuncExecsCountsEveryCommand(t *testing.T) {
	r := fakeRunc(t, `{"id":"c","pid":42,"status":"stopped"}`)
	ctx := context.Background()
	log := filepath.Join(t.TempDir(), "console.log")
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"create", func() error { return r.Create(ctx, "c", t.TempDir(), log, false) }},
		// the log relay of a pipes create is a cat, not a runc
		{"create with log relay", func() error { return r.Create(ctx, "c", t.TempDir(), log, true) }},
		{"start", func() error { return r.Start(ctx, "c") }},
		{"state", func() error {
			st, err := r.State(ctx, "c")
			if err == nil && st.Pid != 42 {
				err = errors.New("state not read from the fake runc")
			}
			return err
		}},
		{"kill", func() error { return r.Kill(ctx, "c", "TERM") }},
		{"delete", func() error { return r.Delete(ctx, "c", true) }},
		// the readiness probe of a bundle with ready.exec
		{"exec", func() error {
			code, _, _, err := r.Exec(ctx, "c", []string{"A=1"}, []string{"true"})
			if err == nil && code != 0 {
				err = errors.New("exec failed")
			}
			return err
		}},
		// one poll: the fake reports the container stopped
		{"wait stopped", func() error {
			if !r.WaitStopped(ctx, "c", time.Second) {
				return errors.New("not stopped")
			}
			return nil
		}},
		{"checkpoint", func() error { _, err := r.run(ctx, "checkpoint", "--leave-running", "c"); return err }},
		{"restore", func() error {
			return r.withStdioEnv(ctx, log, []string{"restore", "--detach", "c"}, []string{"CRIU_CONFIG_FILE=x"})
		}},
		{"version", func() error {
			if v := r.Version(); v != "runc version fake" {
				return errors.New("version: " + v)
			}
			return nil
		}},
	} {
		before := runcExecs.Load()
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := runcExecs.Load() - before; got != 1 {
			t.Errorf("%s counted %d runc execs, want 1", tc.name, got)
		}
	}
}

func TestWithUsage(t *testing.T) {
	r := fakeRunc(t, `{"id":"c","pid":42,"status":"running"}`)
	ctx := context.Background()
	res, err := withUsage(func() (*proto.RunResult, error) {
		if _, err := r.State(ctx, "c"); err != nil {
			return nil, err
		}
		if err := r.Start(ctx, "c"); err != nil {
			return nil, err
		}
		return &proto.RunResult{Timings: proto.Timings{Total: time.Second}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tm := res.Timings
	if tm.RuncExecs != 2 {
		t.Errorf("RuncExecs = %d, want 2", tm.RuncExecs)
	}
	if tm.CPUUser+tm.CPUSys <= 0 || tm.MinorFaults <= 0 {
		t.Errorf("two runc processes used no CPU or faults: %+v", tm)
	}
	if tm.Total != time.Second {
		t.Errorf("Total = %s, the timings the handler set were overwritten", tm.Total)
	}

	boom := errors.New("boom")
	if res, err := withUsage(func() (*proto.RunResult, error) { return nil, boom }); res != nil || err != boom {
		t.Errorf("a failed handler returned %v, %v", res, err)
	}
}
