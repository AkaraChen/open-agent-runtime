package nixeval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Child evaluates by running `oar nix-eval --json --timings` as a child
// process, so an evaluator that dies (an abort in the embedded library, an
// out-of-memory exit) is an error here rather than the end of the caller. The
// child picks the backend (the first available one) unless Backend is set; if
// that evaluator crashes, the next available one is used.
type Child struct {
	// Executable defaults to the running binary.
	Executable string
	Backend    string
	// Log receives the child's stderr as it is written: download progress
	// and evaluator warnings.
	Log io.Writer
}

func (c Child) Name() string { return "child(" + c.Backend + ")" }

// stderrTail keeps the last part of a stream for an error message.
type stderrTail struct {
	buf bytes.Buffer
}

const tailMax = 64 << 10

func (t *stderrTail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > tailMax {
		rest := t.buf.Bytes()[t.buf.Len()-tailMax:]
		var nb bytes.Buffer
		nb.Write(rest)
		t.buf = nb
	}
	return len(p), nil
}

// Eval runs the child and returns the evaluator's JSON.
func (c Child) Eval(ctx context.Context, req Request) ([]byte, error) {
	out, _, err := c.EvalTimed(ctx, req)
	return out, err
}

// EvalTimed is Eval, and also returns where the child spent its time. A child
// that prints the evaluator's JSON alone, as one that predates --timings
// would, has it returned as it is, with zero Timings; a Timings field the
// child leaves out is zero as well.
func (c Child) EvalTimed(ctx context.Context, req Request) ([]byte, Timings, error) {
	raw, err := c.run(ctx, req)
	if err != nil {
		return nil, Timings{}, err
	}
	var t Timed
	if err := json.Unmarshal(raw, &t); err != nil || t.Result == nil {
		return raw, Timings{}, nil
	}
	return t.Result, t.Timings, nil
}

// run starts the child and returns what it printed.
func (c Child) run(ctx context.Context, req Request) ([]byte, error) {
	exe := c.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("locate oar binary: %w", err)
		}
	}
	nixpkgs := req.Nixpkgs
	if nixpkgs == "" {
		nixpkgs = req.NixpkgsDir
	}
	args := []string{"nix-eval", "--json", "--timings", "--system", req.System, "--nixpkgs", nixpkgs}
	if c.Backend != "" {
		args = append(args, "--backend", c.Backend)
	}
	if req.Mirror != "" {
		args = append(args, "--nixpkgs-mirror", req.Mirror)
	}
	if req.Workers > 0 {
		args = append(args, "--workers", strconv.Itoa(req.Workers))
	}
	if req.Expr != "" {
		args = append(args, "--expr", req.Expr)
	} else {
		args = append(args, "--")
		args = append(args, req.Attrs...)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	tail := &stderrTail{}
	if c.Log != nil {
		cmd.Stderr = io.MultiWriter(c.Log, tail)
	} else {
		cmd.Stderr = tail
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	msg := strings.TrimSpace(tail.buf.String())
	var exit *exec.ExitError
	if ok := asExitError(err, &exit); ok && !exit.Exited() {
		// a signal: the evaluator aborted or was killed. With the default
		// backend, the next available one evaluates instead.
		crash := fmt.Errorf("nix evaluator crashed (%s)%s", exit.ProcessState, indent(msg))
		if c.Backend == "" {
			for _, b := range []string{Local, Docker} {
				if ok, _ := Available(b); ok {
					if c.Log != nil {
						fmt.Fprintf(c.Log, "%v\n  evaluating with %s instead\n", crash, b)
					}
					next := c
					next.Backend = b
					return next.run(ctx, req)
				}
			}
		}
		return nil, crash
	}
	if msg == "" {
		msg = err.Error()
	}
	return nil, fmt.Errorf("nix evaluation failed%s", indent(msg))
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	return ":\n  " + strings.ReplaceAll(s, "\n", "\n  ")
}
