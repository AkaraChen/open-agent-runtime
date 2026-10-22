//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
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

// runcExecs counts the runc processes the agent has started, readiness
// probes (`runc exec`) and state polls included. Every runc command is built
// here and run right away, so this is the one place that counts them;
// Version, which runs runc without --root, counts itself. The per-request
// share goes into proto.Timings (see usage).
var runcExecs atomic.Int64

func (r *Runc) cmd(ctx context.Context, args ...string) *exec.Cmd {
	runcExecs.Add(1)
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
		st, err := r.State(ctx, id)
		if err == nil && (st.Status == "stopped" || st.Status == "deleted") {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Version returns runc's version line.
func (r *Runc) Version() string {
	runcExecs.Add(1)
	out, err := exec.Command(r.Binary, "--version").Output()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return line
}
