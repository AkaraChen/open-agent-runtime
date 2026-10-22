//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Runc wraps the runc command line.
type Runc struct {
	Binary  string
	Root    string // runc --root state directory
	NoPivot bool   // required when the guest root is an initramfs
}

type runcState struct {
	ID     string `json:"id"`
	Pid    int    `json:"pid"`
	Status string `json:"status"`
	Bundle string `json:"bundle"`
}

func (r *Runc) cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, r.Binary, append([]string{"--root", r.Root}, args...)...)
}

func (r *Runc) run(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	c := r.cmd(ctx, args...)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("runc %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Create runs `runc create`. With pipes the container's stdio are pipes
// feeding a log relay process: pipes (unlike a host file) can be
// checkpointed and re-attached by CRIU, so snapshot-enabled bundles need
// them. Otherwise the container writes the log file directly, which avoids
// spawning a relay per container.
func (r *Runc) Create(ctx context.Context, id, bundle, logPath string, pipes bool) error {
	// no --pid-file: runc writes it with O_SYNC, which waits for an ext4
	// journal commit; after a warm that commit carries the metadata of
	// every extracted file and took up to 3 s (the pid comes from state)
	args := []string{"create", "--bundle", bundle}
	if r.NoPivot {
		args = append(args, "--no-pivot")
	}
	args = append(args, id)
	if pipes {
		return r.withStdio(ctx, logPath, args)
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	c := r.cmd(ctx, args...)
	c.Stdin, c.Stdout, c.Stderr = devnull, log, log
	off, _ := log.Seek(0, 2)
	if err := c.Run(); err != nil {
		tail := make([]byte, 4096)
		n, _ := log.ReadAt(tail, off)
		return fmt.Errorf("runc create: %w: %s", err, strings.TrimSpace(string(tail[:n])))
	}
	return nil
}

// withStdio runs a runc command whose container process inherits pipe
// stdio connected to a log relay.
func (r *Runc) withStdio(ctx context.Context, logPath string, args []string) error {
	return r.withStdioEnv(ctx, logPath, args, nil)
}

func (r *Runc) withStdioEnv(ctx context.Context, logPath string, args, env []string) error {
	out, err := startLogRelay(logPath)
	if err != nil {
		return err
	}
	defer out.Close()
	inR, inW, err := os.Pipe()
	if err != nil {
		return err
	}
	inW.Close() // stdin reads EOF
	defer inR.Close()
	c := r.cmd(ctx, args...)
	if len(env) > 0 {
		c.Env = append(os.Environ(), env...)
	}
	c.Stdin, c.Stdout, c.Stderr = inR, out, out
	if err := c.Run(); err != nil {
		// runc reports its own errors on the same stderr; give the relay a
		// moment to write them, then quote the tail of the log
		time.Sleep(50 * time.Millisecond)
		tail, _ := os.ReadFile(logPath)
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		return fmt.Errorf("runc %s: %w: %s", args[0], err, strings.TrimSpace(string(tail)))
	}
	return nil
}

// startLogRelay starts `cat >> logPath` reading from a pipe and returns the
// pipe's write end.
func startLogRelay(logPath string) (*os.File, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	relay := exec.Command("cat")
	relay.Stdin, relay.Stdout = pr, logf
	relay.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := relay.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("start log relay: %w", err)
	}
	pr.Close()
	go relay.Wait()
	return pw, nil
}

// Start runs `runc start`.
func (r *Runc) Start(ctx context.Context, id string) error {
	_, err := r.run(ctx, "start", id)
	return err
}

// State returns the runc state, or status "stopped" when unknown to runc.
func (r *Runc) State(ctx context.Context, id string) (runcState, error) {
	out, err := r.run(ctx, "state", id)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not exist") {
			return runcState{ID: id, Status: "deleted"}, nil
		}
		return runcState{}, err
	}
	var st runcState
	if err := json.Unmarshal(out, &st); err != nil {
		return runcState{}, err
	}
	return st, nil
}

// initProc is a container's init process as libcontainer recorded it in
// state.json.
type initProc struct {
	pid   int
	start uint64 // clock ticks since boot: tells the process from a later user of its pid
}

// StateFile returns what State returns, from the files libcontainer keeps
// instead of by running `runc state`.
//
// PERF: a runc invocation is a fork+exec of a ~10 MiB static binary: ~4.6 ms
// on KVM, ~100 ms on vz, ~250 ms on TCG. Observing a container (after every
// create and start, on every readiness poll, while stopping it) only needs to
// know whether its init process is alive, and libcontainer records exactly
// that when it creates or restores a container: <root>/<id>/state.json holds
// the init process's pid (init_process_pid) and start time
// (init_process_start, field 22 of /proc/<pid>/stat, which is how runc tells
// a reused pid). This is runc's own derivation of the status
// (Container.runType):
//
//	no state.json                                      deleted ("container does not exist")
//	/proc/<pid>/stat gone, other start time, Z or X    stopped
//	<id>/exec.fifo exists (`runc start` removes it)    created
//	otherwise                                          running
//
// state.json is libcontainer's format; these three fields are the same in
// runc 1.4 and 1.5 and have been since 1.0. Anything unexpected is returned
// as an error, and FastState then asks runc. Not modelled: "paused" (a
// frozen cgroup), which the agent never uses.
func (r *Runc) StateFile(id string) (runcState, error) {
	ip, err := r.readInit(id)
	if errors.Is(err, fs.ErrNotExist) {
		return runcState{ID: id, Status: "deleted"}, nil
	}
	if err != nil {
		return runcState{}, err
	}
	alive, err := ip.alive()
	if err != nil {
		return runcState{}, err
	}
	if !alive {
		return runcState{ID: id, Status: "stopped"}, nil // like runc, without a pid
	}
	st := runcState{ID: id, Pid: ip.pid}
	switch _, err := os.Stat(filepath.Join(r.Root, id, "exec.fifo")); {
	case err == nil:
		st.Status = "created"
	case errors.Is(err, fs.ErrNotExist):
		st.Status = "running"
	default:
		return runcState{}, err
	}
	return st, nil
}

// FastState is State without the runc exec on the normal path: it reads the
// state files (StateFile) and only asks runc when they are not as expected,
// for instance with a runc whose state.json differs. That is logged once.
func (r *Runc) FastState(ctx context.Context, id string) (runcState, error) {
	st, err := r.StateFile(id)
	if err == nil {
		return st, nil
	}
	warnOnce("state-file", "cannot read container state from runc's files (%v); asking \"runc state\" instead", err)
	return r.State(ctx, id)
}

// readInit reads the init process of a container from its state.json. A
// container runc does not know has no file: the error then satisfies
// errors.Is(err, fs.ErrNotExist).
func (r *Runc) readInit(id string) (initProc, error) {
	if r.Root == "" || id == "" || strings.ContainsRune(id, filepath.Separator) {
		return initProc{}, fmt.Errorf("no state directory for container %q in %q", id, r.Root)
	}
	f, err := os.Open(filepath.Join(r.Root, id, "state.json"))
	if err != nil {
		return initProc{}, err
	}
	defer f.Close()
	ip, err := parseInit(f, id)
	if err != nil {
		return initProc{}, fmt.Errorf("%s: %w", f.Name(), err)
	}
	return ip, nil
}

// parseInit extracts the init process from libcontainer's state.json. The
// file embeds the container's whole configuration (a few KiB), but the three
// fields come first, so decoding stops after them.
func parseInit(rd io.Reader, id string) (initProc, error) {
	dec := json.NewDecoder(rd)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return initProc{}, errors.New("not a JSON object")
	}
	var (
		ip    initProc
		gotID string
		found int
	)
	for found < 3 && dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return initProc{}, err
		}
		var dst any
		wanted := true
		switch tok {
		case "id":
			dst = &gotID
		case "init_process_pid":
			dst = &ip.pid
		case "init_process_start":
			dst = &ip.start
		default:
			dst, wanted = new(json.RawMessage), false // another field: skip its value
		}
		if err := dec.Decode(dst); err != nil {
			return initProc{}, err
		}
		if wanted {
			found++
		}
	}
	if ip.pid <= 0 || ip.start == 0 {
		return initProc{}, errors.New("no init process recorded")
	}
	if gotID != "" && gotID != id {
		return initProc{}, fmt.Errorf("belongs to container %q", gotID)
	}
	return ip, nil
}

// alive reports whether the recorded init process still exists: it must have
// the recorded start time (else its pid was reused) and must not be a zombie
// or dead, which runc treats as stopped, too.
func (ip initProc) alive() (bool, error) {
	state, start, err := procStat(ip.pid)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	return start == ip.start && state != 'Z' && state != 'X' && state != 'x', nil
}

// procStat returns the state (field 3) and the start time (field 22) of a
// process.
func procStat(pid int) (state byte, start uint64, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, err
	}
	return parseProcStat(b)
}

// parseProcStat parses /proc/<pid>/stat: "pid (comm) state ppid ...". The
// command name may contain spaces and parentheses, so the fields after it
// start behind the last ')'.
func parseProcStat(b []byte) (state byte, start uint64, err error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, errors.New("malformed stat: no command name")
	}
	rest := b[i+1:]
	for n := 1; ; n++ { // n counts from field 3, the state
		rest = bytes.TrimLeft(rest, " ")
		if len(rest) == 0 {
			return 0, 0, errors.New("malformed stat: too few fields")
		}
		field := rest
		if j := bytes.IndexByte(rest, ' '); j >= 0 {
			field, rest = rest[:j], rest[j:]
		} else {
			rest = nil
		}
		switch n {
		case 1:
			state = field[0]
		case 20: // field 22, starttime
			start, err = strconv.ParseUint(string(bytes.TrimSpace(field)), 10, 64)
			return state, start, err
		}
	}
}

var warned sync.Map

// warnOnce writes a line to the agent's stderr (the guest console) the first
// time it is called with key.
func warnOnce(key, format string, args ...any) {
	if _, seen := warned.LoadOrStore(key, true); !seen {
		fmt.Fprintf(os.Stderr, "oar-agent: %s\n", fmt.Sprintf(format, args...))
	}
}

// Kill sends a signal to the container init.
func (r *Runc) Kill(ctx context.Context, id, sig string) error {
	_, err := r.run(ctx, "kill", id, sig)
	return err
}

// Delete removes the container.
func (r *Runc) Delete(ctx context.Context, id string, force bool) error {
	args := []string{"delete"}
	if force {
		args = append(args, "--force")
	}
	_, err := r.run(ctx, append(args, id)...)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		return nil
	}
	return err
}

// Exec runs a process inside a running container and captures its output.
func (r *Runc) Exec(ctx context.Context, id string, env, args []string) (int, string, string, error) {
	a := []string{"exec"}
	for _, e := range env {
		a = append(a, "--env", e)
	}
	a = append(a, id)
	a = append(a, args...)
	c := r.cmd(ctx, a...)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return -1, stdout.String(), stderr.String(), err
		}
		code = ee.ExitCode()
	}
	return code, stdout.String(), stderr.String(), nil
}

// WaitStopped polls until the container is stopped or the timeout expires.
func (r *Runc) WaitStopped(ctx context.Context, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		// PERF: look at the state files (see StateFile); only a container
		// whose files cannot be read costs a `runc state` per poll
		pause := 2 * time.Millisecond
		st, err := r.StateFile(id)
		if err != nil {
			st, err = r.State(ctx, id)
			pause = 50 * time.Millisecond
		}
		if err == nil && (st.Status == "stopped" || st.Status == "deleted") {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(pause)
	}
}

// Version returns runc's version line.
func (r *Runc) Version() string {
	out, err := exec.Command(r.Binary, "--version").Output()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return line
}
