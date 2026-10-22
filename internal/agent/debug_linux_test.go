//go:build linux

package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// skipIfTestProfiling skips tests that start a profiler of their own while
// go test is already running one.
func skipIfTestProfiling(t *testing.T) {
	t.Helper()
	for _, name := range []string{"test.cpuprofile", "test.trace", "test.blockprofile", "test.mutexprofile"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			t.Skipf("go test is writing -%s", strings.TrimPrefix(name, "test."))
		}
	}
}

// pipeClient serves h over in-memory connections, one per request.
func pipeClient(t *testing.T, h proto.Handler) *proto.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &proto.Client{Dial: func(context.Context) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go proto.Serve(ctx, c2, h)
		return c1, nil
	}}
}

// shortWindow serves a sampling profile of kind that runs for d, which is
// shorter than the whole seconds the protocol can ask for.
func shortWindow(kind string, d time.Duration) proto.Handler {
	return func(ctx context.Context, _ *proto.Request, _ io.Reader, rs *proto.ResponseStream) (any, error) {
		return nil, sampleProfile(ctx, kind, d, rs)
	}
}

func fetchProfile(t *testing.T, c *proto.Client, args proto.DebugProfileArgs) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	err := c.CallBody(context.Background(), proto.OpDebugProfile, args, nil, 0, nil, &buf)
	return buf.Bytes(), err
}

// requirePprof checks that b is a gzip stream with content, which is how
// runtime/pprof writes a profile.
func requirePprof(t *testing.T, kind string, b []byte) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: %d bytes are not a gzip stream: %v", kind, len(b), err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil || len(raw) == 0 {
		t.Fatalf("%s: gzip stream holds %d bytes, err %v", kind, len(raw), err)
	}
}

// waitSampling waits until a sampling profile is running.
func waitSampling(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if !sampling.TryLock() {
			return
		}
		sampling.Unlock()
	}
	t.Fatal("no sampling profile started")
}

func TestProfileDuration(t *testing.T) {
	for in, want := range map[int]time.Duration{
		0:      10 * time.Second,
		-3:     10 * time.Second,
		1:      time.Second,
		30:     30 * time.Second,
		120:    120 * time.Second,
		121:    120 * time.Second,
		999999: 120 * time.Second,
	} {
		if got := profileDuration(in); got != want {
			t.Errorf("profileDuration(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestDebugProfileInstant(t *testing.T) {
	c := pipeClient(t, (&Server{}).Handle)
	for _, kind := range []string{proto.ProfileHeap, proto.ProfileAllocs, proto.ProfileGoroutine} {
		b, err := fetchProfile(t, c, proto.DebugProfileArgs{Kind: kind})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		requirePprof(t, kind, b)
	}
}

func TestDebugProfileUnknownKind(t *testing.T) {
	c := pipeClient(t, (&Server{}).Handle)
	_, err := fetchProfile(t, c, proto.DebugProfileArgs{Kind: "threadcreate"})
	var re *proto.RemoteError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, `unknown profile kind "threadcreate"`) {
		t.Fatalf("got %v, want a remote error naming the kind", err)
	}
}

// TestDebugProfileCPU goes through the whole op, seconds included.
func TestDebugProfileCPU(t *testing.T) {
	skipIfTestProfiling(t)
	c := pipeClient(t, (&Server{}).Handle)
	start := time.Now()
	b, err := fetchProfile(t, c, proto.DebugProfileArgs{Kind: proto.ProfileCPU, Seconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("a one second cpu profile returned after %s", d)
	}
	requirePprof(t, proto.ProfileCPU, b)
}

func TestDebugProfileTrace(t *testing.T) {
	skipIfTestProfiling(t)
	c := pipeClient(t, shortWindow(proto.ProfileTrace, 200*time.Millisecond))
	b, err := fetchProfile(t, c, proto.DebugProfileArgs{Kind: proto.ProfileTrace})
	if err != nil {
		t.Fatal(err)
	}
	// "go 1.NN trace" opens an execution trace
	if len(b) < 16 || !bytes.HasPrefix(b, []byte("go 1.")) || !bytes.Contains(b[:16], []byte(" trace")) {
		t.Fatalf("not an execution trace: %d bytes starting %q", len(b), b[:min(len(b), 16)])
	}
}

// contend makes one goroutine wait 20 ms for a mutex: the block profile
// records the wait, the mutex profile the unlock that ended it.
func contend() {
	var mu sync.Mutex
	mu.Lock()
	done := make(chan struct{})
	go func() { mu.Lock(); mu.Unlock(); close(done) }()
	time.Sleep(20 * time.Millisecond)
	mu.Unlock()
	<-done
}

// events is the number of contention events a profile of the runtime holds.
func events(profile func([]runtime.BlockProfileRecord) (int, bool)) (n int64) {
	recs := make([]runtime.BlockProfileRecord, 64)
	k, _ := profile(recs)
	for _, r := range recs[:min(k, len(recs))] {
		n += r.Count
	}
	return n
}

// TestDebugProfileBlockMutex checks that these profiles, which the runtime
// leaves off, record the contention of their window and nothing after it.
func TestDebugProfileBlockMutex(t *testing.T) {
	skipIfTestProfiling(t)
	for _, tc := range []struct {
		kind    string
		profile func([]runtime.BlockProfileRecord) (int, bool)
	}{
		{proto.ProfileBlock, runtime.BlockProfile},
		{proto.ProfileMutex, runtime.MutexProfile},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			before := events(tc.profile)
			c := pipeClient(t, shortWindow(tc.kind, 300*time.Millisecond))
			stop, stopped := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(stopped)
				for {
					select {
					case <-stop:
						return
					default:
						contend()
					}
				}
			}()
			b, err := fetchProfile(t, c, proto.DebugProfileArgs{Kind: tc.kind})
			close(stop)
			<-stopped
			if err != nil {
				t.Fatal(err)
			}
			requirePprof(t, tc.kind, b)
			during := events(tc.profile)
			if during <= before {
				t.Errorf("recorded no %s events during the window", tc.kind)
			}
			contend()
			if after := events(tc.profile); after != during {
				t.Errorf("%s sampling is still on after the window: %d events became %d", tc.kind, during, after)
			}
		})
	}
}

// TestDebugProfileBusy checks that a second sampling profile is refused
// instead of disturbing the first.
func TestDebugProfileBusy(t *testing.T) {
	skipIfTestProfiling(t)
	first := pipeClient(t, shortWindow(proto.ProfileMutex, 500*time.Millisecond))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := fetchProfile(t, first, proto.DebugProfileArgs{Kind: proto.ProfileMutex}); err != nil {
			t.Errorf("first profile: %v", err)
		}
	}()
	defer wg.Wait()
	waitSampling(t)
	second := pipeClient(t, shortWindow(proto.ProfileCPU, time.Second))
	_, err := fetchProfile(t, second, proto.DebugProfileArgs{Kind: proto.ProfileCPU})
	var re *proto.RemoteError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "already running") {
		t.Fatalf("got %v, want an already running error", err)
	}
}
