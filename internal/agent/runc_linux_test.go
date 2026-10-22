//go:build linux

package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the process of a container: started with
// OAR_TEST_PROCESS it reports "ready" on stdout and then sleeps (and, in the
// ignore-term mode, ignores SIGTERM).
func init() {
	switch os.Getenv("OAR_TEST_PROCESS") {
	case "sleep":
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
	default:
		return
	}
	fmt.Println("ready")
	time.Sleep(time.Minute)
	os.Exit(0)
}

// startProcess starts a real process (see init) and waits until it runs. It
// is killed and reaped at the end of the test.
func startProcess(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "OAR_TEST_PROCESS="+mode)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("test process did not start: %q %v", line, err)
	}
	return cmd
}

// statLine builds a /proc/<pid>/stat line with 52 fields.
func statLine(pid int, comm string, state byte, start uint64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%d (%s) %c", pid, comm, state)
	for field := 4; field <= 52; field++ {
		if field == 22 {
			fmt.Fprintf(&b, " %d", start)
		} else {
			b.WriteString(" 0")
		}
	}
	b.WriteString("\n")
	return []byte(b.String())
}

func TestParseProcStat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		line  []byte
		state byte
		start uint64
	}{
		{"plain", statLine(7, "sleep", 'S', 41344799), 'S', 41344799},
		{"zombie", statLine(7, "sleep", 'Z', 5), 'Z', 5},
		{"runc init", statLine(7, "runc:[2:INIT]", 'S', 41344799), 'S', 41344799},
		{"spaces and parentheses in the command", statLine(7, "a b) (c ) d", 'R', 123456789012), 'R', 123456789012},
		{"empty command", statLine(7, "", 'S', 9), 'S', 9},
	} {
		state, start, err := parseProcStat(tc.line)
		if err != nil || state != tc.state || start != tc.start {
			t.Errorf("%s: got %c %d %v, want %c %d", tc.name, state, start, err, tc.state, tc.start)
		}
	}
	for name, line := range map[string]string{
		"empty":           "",
		"no command":      "7 sleep S 1 2",
		"too few fields":  "7 (sleep) S 1 2 3",
		"start not a num": "7 (x) S" + strings.Repeat(" 0", 18) + " abc 0 0 0",
		"nothing behind":  "7 (sleep)",
	} {
		if _, _, err := parseProcStat([]byte(line)); err == nil {
			t.Errorf("%s: parsed without an error", name)
		}
	}
	// the real thing: this process is alive (its main thread may be asleep
	// while another runs the test), and its start time is stable
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	state, start, err := parseProcStat(b)
	if err != nil || (state != 'R' && state != 'S') || start == 0 {
		t.Fatalf("/proc/self/stat: got %c %d %v", state, start, err)
	}
	if _, again, _ := procStat(os.Getpid()); again != start {
		t.Fatalf("start time of this process changed: %d then %d", start, again)
	}
}

func TestParseInit(t *testing.T) {
	// libcontainer's layout: the init process first, then the configuration
	// (whose nested fields must not be mistaken for the top-level ones)
	const real = `{"id":"c1","init_process_pid":123,"init_process_start":456,"created":"2026-10-22T03:20:46.061643794Z",` +
		`"config":{"rootfs":"/b/rootfs","labels":["bundle=/b"],"init_process_pid":999,"id":"other"},` +
		`"rootless":false,"cgroup_paths":{"":"/sys/fs/cgroup/oar/c1"},"namespace_paths":{"NEWPID":"/proc/123/ns/pid"}}`
	for name, tc := range map[string]struct {
		doc   string
		pid   int
		start uint64
		err   bool
	}{
		"libcontainer":       {doc: real, pid: 123, start: 456},
		"other order":        {doc: `{"config":{"id":"other","init_process_pid":999},"init_process_start":456,"id":"c1","init_process_pid":123}`, pid: 123, start: 456},
		"no id":              {doc: `{"init_process_pid":123,"init_process_start":456}`, pid: 123, start: 456},
		"large start time":   {doc: `{"id":"c1","init_process_pid":4194304,"init_process_start":9007199254740993}`, pid: 4194304, start: 9007199254740993},
		"other container":    {doc: `{"id":"zzz","init_process_pid":123,"init_process_start":456}`, err: true},
		"no init process":    {doc: `{"id":"c1","created":"x"}`, err: true},
		"no start time":      {doc: `{"id":"c1","init_process_pid":123}`, err: true},
		"pid zero":           {doc: `{"id":"c1","init_process_pid":0,"init_process_start":456}`, err: true},
		"pid is a string":    {doc: `{"id":"c1","init_process_pid":"123","init_process_start":456}`, err: true},
		"truncated":          {doc: real[:40], err: true},
		"empty":              {doc: ``, err: true},
		"not an object":      {doc: `[1,2,3]`, err: true},
		"not json":           {doc: `container 123 is running`, err: true},
		"nested fields only": {doc: `{"config":{"id":"c1","init_process_pid":123,"init_process_start":456}}`, err: true},
	} {
		ip, err := parseInit(strings.NewReader(tc.doc), "c1")
		if (err != nil) != tc.err || err == nil && (ip.pid != tc.pid || ip.start != tc.start) {
			t.Errorf("%s: got %+v %v, want pid %d start %d error %v", name, ip, err, tc.pid, tc.start, tc.err)
		}
	}
}

// writeState writes a state.json in libcontainer's layout for an init process.
func writeState(t *testing.T, root, id string, pid int, start uint64) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"id":%q,"init_process_pid":%d,"init_process_start":%d,"created":"2026-10-22T03:20:46.061643794Z",`+
		`"config":{"rootfs":"/b/rootfs","labels":["bundle=/b"]},"rootless":false,"cgroup_paths":{"":"/sys/fs/cgroup/oar/%s"}}`,
		id, pid, start, id)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeRunc is a runc binary that records its arguments and answers `state`.
func fakeRunc(t *testing.T) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "runc"), filepath.Join(dir, "calls")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\ncase \"$3\" in state) echo '{\"id\":\"fake\",\"pid\":42,\"status\":\"running\"}' ;; esac\n", calls)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// execs returns the runc invocations a fakeRunc recorded.
func execs(t *testing.T, calls string) []string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// waitState waits until the process is in the given state in /proc.
func waitState(t *testing.T, pid int, want byte) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if state, _, err := procStat(pid); err == nil && state == want {
			return
		}
	}
	t.Fatalf("process %d did not reach state %c", pid, want)
}

func TestStateFile(t *testing.T) {
	root := t.TempDir()
	r := Runc{Binary: "/nonexistent/runc", Root: root} // never exec'd
	child := startProcess(t, "sleep")
	pid := child.Process.Pid
	_, start, err := procStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	writeState(t, root, "c1", pid, start)
	want := func(id, status string, wantPid int) {
		t.Helper()
		st, err := r.StateFile(id)
		if err != nil || st.Status != status || st.Pid != wantPid || st.ID != id {
			t.Fatalf("%s: got %+v %v, want status %s pid %d", id, st, err, status, wantPid)
		}
	}

	// runc create leaves exec.fifo until runc start removes it
	fifo := filepath.Join(root, "c1", "exec.fifo")
	if err := syscall.Mkfifo(fifo, 0o622); err != nil {
		t.Fatal(err)
	}
	want("c1", "created", pid)
	os.Remove(fifo)
	want("c1", "running", pid)

	// a container runc does not know
	want("nosuch", "deleted", 0)

	// the pid was reused by another process: it started at another time
	writeState(t, root, "reused", pid, start+1)
	want("reused", "stopped", 0)

	// the init process died but is not reaped yet: a zombie is stopped
	child.Process.Kill()
	waitState(t, pid, 'Z')
	want("c1", "stopped", 0)

	// reaped: gone from /proc
	child.Wait()
	want("c1", "stopped", 0)
}

func TestStateFileUnreadable(t *testing.T) {
	root := t.TempDir()
	r := Runc{Root: root}
	for name, doc := range map[string]string{
		"garbage":      "container 123 is running",
		"empty":        "",
		"other fields": `{"id":"c1","state":"running"}`,
	} {
		os.MkdirAll(filepath.Join(root, "c1"), 0o755)
		os.WriteFile(filepath.Join(root, "c1", "state.json"), []byte(doc), 0o644)
		if st, err := r.StateFile("c1"); err == nil {
			t.Errorf("%s: read %+v from an unusable state.json", name, st)
		}
	}
	for _, id := range []string{"", "a/b", "../c1"} {
		if st, err := r.StateFile(id); err == nil {
			t.Errorf("id %q: got %+v, want an error", id, st)
		}
	}
	if st, err := (&Runc{}).StateFile("c1"); err == nil {
		t.Errorf("no runc root: got %+v, want an error", st)
	}
}

func TestFastState(t *testing.T) {
	root := t.TempDir()
	bin, calls := fakeRunc(t)
	r := Runc{Binary: bin, Root: root}
	child := startProcess(t, "sleep")
	_, start, _ := procStat(child.Process.Pid)
	ctx := context.Background()

	// the state files are enough: runc is not run
	writeState(t, root, "c1", child.Process.Pid, start)
	if st, err := r.FastState(ctx, "c1"); err != nil || st.Status != "running" || st.Pid != child.Process.Pid {
		t.Fatalf("got %+v %v", st, err)
	}
	if st, err := r.FastState(ctx, "nosuch"); err != nil || st.Status != "deleted" {
		t.Fatalf("got %+v %v", st, err)
	}
	if got := execs(t, calls); len(got) != 0 {
		t.Fatalf("runc was run for a readable state: %q", got)
	}

	// a state.json the agent cannot make sense of (another runc version):
	// ask runc
	os.MkdirAll(filepath.Join(root, "c2"), 0o755)
	os.WriteFile(filepath.Join(root, "c2", "state.json"), []byte(`{"format":"v2"}`), 0o644)
	st, err := r.FastState(ctx, "c2")
	if err != nil || st.Status != "running" || st.Pid != 42 {
		t.Fatalf("fallback: got %+v %v", st, err)
	}
	if got, want := execs(t, calls), []string{"--root " + root + " state c2"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("runc calls: got %q, want %q", got, want)
	}
}

func TestWaitStoppedPolls(t *testing.T) {
	root := t.TempDir()
	bin, calls := fakeRunc(t)
	r := Runc{Binary: bin, Root: root}
	child := startProcess(t, "sleep")
	_, start, _ := procStat(child.Process.Pid)
	writeState(t, root, "c1", child.Process.Pid, start)
	ctx := context.Background()

	// still running: the timeout expires
	t0 := time.Now()
	if r.WaitStopped(ctx, "c1", 100*time.Millisecond) {
		t.Fatal("a running container was reported stopped")
	}
	if d := time.Since(t0); d < 100*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %s for a 100ms timeout", d)
	}

	// it stops while we wait
	go func() { time.Sleep(50 * time.Millisecond); child.Process.Kill() }()
	t0 = time.Now()
	if !r.WaitStopped(ctx, "c1", 10*time.Second) {
		t.Fatal("the stopped container was not noticed")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("noticed the stop after %s", d)
	}
	child.Wait()

	// a container that is already gone, or that runc does not know
	if !r.WaitStopped(ctx, "c1", time.Second) || !r.WaitStopped(ctx, "nosuch", time.Second) {
		t.Fatal("a stopped or deleted container was reported running")
	}
	if got := execs(t, calls); len(got) != 0 {
		t.Fatalf("runc was run while polling the state files: %q", got)
	}

	// a cancelled context ends the wait
	child2 := startProcess(t, "sleep")
	_, start2, _ := procStat(child2.Process.Pid)
	writeState(t, root, "c2", child2.Process.Pid, start2)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if r.WaitStopped(cctx, "c2", time.Minute) {
		t.Fatal("a cancelled wait reported the container stopped")
	}
}

func TestObserve(t *testing.T) {
	root := t.TempDir()
	bin, calls := fakeRunc(t)
	s := &Server{cfg: Config{Runc: Runc{Binary: bin, Root: root}}}
	child := startProcess(t, "sleep")
	_, start, _ := procStat(child.Process.Pid)
	writeState(t, root, "web-1", child.Process.Pid, start)
	in := &instance{Name: "web", ID: "web-1", BundleDigest: "sha256:x", dir: "/b/web-1"}
	ctx := context.Background()

	c := s.observe(ctx, in)
	if c.Status != "running" || c.Pid != child.Process.Pid || c.Name != "web" || c.ID != "web-1" || c.Bundle != "/b/web-1" {
		t.Fatalf("running: got %+v", c)
	}
	child.Process.Kill()
	child.Wait()
	if c := s.observe(ctx, in); c.Status != "stopped" || c.Pid != 0 {
		t.Fatalf("stopped: got %+v", c)
	}
	if c := s.observe(ctx, &instance{Name: "x", ID: "x-1"}); c.Status != "deleted" {
		t.Fatalf("unknown: got %+v", c)
	}
	if got := execs(t, calls); len(got) != 0 {
		t.Fatalf("observe ran runc: %q", got)
	}
}

// BenchmarkStateFile is the cost that replaces a runc exec (4.6 ms on KVM).
func BenchmarkStateFile(b *testing.B) {
	root := b.TempDir()
	_, start, err := procStat(os.Getpid())
	if err != nil {
		b.Fatal(err)
	}
	dir := filepath.Join(root, "c1")
	os.MkdirAll(dir, 0o755)
	// as big as libcontainer's: the configuration of a container follows the pid
	doc := fmt.Sprintf(`{"id":"c1","init_process_pid":%d,"init_process_start":%d,"created":"2026-10-22T03:20:46Z","config":{"mounts":[%s]}}`,
		os.Getpid(), start, strings.TrimSuffix(strings.Repeat(`{"source":"proc","destination":"/proc","device":"proc","flags":14},`, 60), ","))
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(doc), 0o644)
	r := Runc{Root: root}
	b.ReportAllocs()
	for b.Loop() {
		if st, err := r.StateFile("c1"); err != nil || st.Status != "running" {
			b.Fatalf("got %+v %v", st, err)
		}
	}
}
