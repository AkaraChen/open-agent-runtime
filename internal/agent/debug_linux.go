//go:build linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sync"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// The profiles that sample over time run for the requested seconds; the
// default and the limit keep a forgotten request from holding a profile
// open.
const (
	defaultProfileSeconds = 10
	maxProfileSeconds     = 120
)

func profileDuration(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = defaultProfileSeconds
	}
	return time.Duration(min(seconds, maxProfileSeconds)) * time.Second
}

// sampling is held for the whole run of a profile that samples over time:
// the runtime has one CPU profile, one execution trace and one block and
// mutex sampling rate per process, so two such requests would break each
// other.
var sampling sync.Mutex

// debugProfile answers debug.profile: it writes a runtime profile of the
// agent process as the response body. The native backend runs the agent in
// the supervisor, so there it profiles the supervisor.
func debugProfile(ctx context.Context, a proto.DebugProfileArgs, rs *proto.ResponseStream) error {
	switch a.Kind {
	case proto.ProfileHeap, proto.ProfileAllocs:
		// both report the state as of the last completed collection, which
		// can be a long time ago in a mostly idle agent
		runtime.GC()
		return writeProfile(a.Kind, rs)
	case proto.ProfileGoroutine:
		return writeProfile(a.Kind, rs)
	case proto.ProfileCPU, proto.ProfileTrace, proto.ProfileBlock, proto.ProfileMutex:
		return sampleProfile(ctx, a.Kind, profileDuration(a.Seconds), rs)
	}
	return fmt.Errorf("unknown profile kind %q (want cpu, heap, allocs, goroutine, block, mutex or trace)", a.Kind)
}

// writeProfile sends the named runtime/pprof profile. It is small, so it is
// built first and sent with its size: a failure is still an error response.
func writeProfile(name string, rs *proto.ResponseStream) error {
	p := pprof.Lookup(name)
	if p == nil {
		return fmt.Errorf("no %s profile", name)
	}
	var buf bytes.Buffer
	if err := p.WriteTo(&buf, 0); err != nil {
		return err
	}
	w, err := rs.Begin(nil, int64(buf.Len()))
	if err != nil {
		return err
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// sampleProfile runs a profile that samples over time (cpu, trace, block,
// mutex) for d.
func sampleProfile(ctx context.Context, kind string, d time.Duration, rs *proto.ResponseStream) error {
	if !sampling.TryLock() {
		return fmt.Errorf("another cpu, trace, block or mutex profile is already running")
	}
	defer sampling.Unlock()
	switch kind {
	case proto.ProfileCPU:
		return streamProfile(ctx, d, rs, pprof.StartCPUProfile, pprof.StopCPUProfile)
	case proto.ProfileTrace:
		return streamProfile(ctx, d, rs, trace.Start, trace.Stop)
	case proto.ProfileBlock:
		// every event, as go test -blockprofile does; only for this window
		runtime.SetBlockProfileRate(1)
		defer runtime.SetBlockProfileRate(0)
	case proto.ProfileMutex:
		runtime.SetMutexProfileFraction(1)
		defer runtime.SetMutexProfileFraction(0)
	}
	// block and mutex profiling is off in the runtime unless enabled, so
	// these profiles hold what was recorded during this window (and earlier
	// ones)
	if err := sleepCtx(ctx, d); err != nil {
		return err
	}
	return writeProfile(kind, rs)
}

// streamProfile runs a profiler that writes into the response while it
// runs (a trace can be large) for d.
func streamProfile(ctx context.Context, d time.Duration, rs *proto.ResponseStream, start func(io.Writer) error, stop func()) error {
	w, err := rs.Begin(nil, -1)
	if err != nil {
		return err
	}
	// only sampleProfile starts these while it holds sampling, so starting
	// cannot fail from here on (the response has begun, an error could not
	// be reported any more)
	if err := start(w); err != nil {
		return err
	}
	err = sleepCtx(ctx, d)
	stop() // returns after the last write
	return err
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
