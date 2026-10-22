//go:build linux

package agent

import (
	"time"

	"golang.org/x/sys/unix"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// usage is a reading of what the agent process has used so far: the runc
// processes it started, and the CPU time and page faults of the process
// itself and of the children it has waited for (runc, cp, cat). The
// difference of two readings is the share of a request, which run, reload
// and prepare report in proto.Timings and compose in proto.ComposeResult.
// CPU time is far steadier than wall time and tells a request that is CPU
// bound from one that waits; the runc count does not depend on the runner
// at all.
//
// PERF: the counters are process-wide, not per request. Concurrent requests
// share them, and so does work that no request waits for: the cleanup of a
// retired instance after a reload (its runc delete starts at once and is
// counted in that reload, its CPU time mostly lands in the next request),
// the ingress proxy, the garbage collector, and on the native backend the
// rest of the supervisor. The figures are exact for one request at a time,
// which is how the benchmarks run. A start runs three runc commands (create,
// start, state); a switch adds kill, delete and a state poll every 50 ms
// while the old instance exits, so its count varies by one or two. Counting
// per request would have to thread a handle through every Runc call; two
// getrusage calls and an atomic load per request cost a few microseconds. A
// child counts once it has exited and been waited for, and on kernels
// without precise CPU accounting only the sum of user and system time is
// exact, not the split.
type usage struct {
	runcExecs      int64
	user, sys      time.Duration
	minflt, majflt int64
}

// readUsage takes a reading.
func readUsage() usage {
	var self, kids unix.Rusage
	// neither call can fail with these arguments
	unix.Getrusage(unix.RUSAGE_SELF, &self)
	unix.Getrusage(unix.RUSAGE_CHILDREN, &kids)
	return usage{
		runcExecs: runcExecs.Load(),
		user:      time.Duration(self.Utime.Nano() + kids.Utime.Nano()),
		sys:       time.Duration(self.Stime.Nano() + kids.Stime.Nano()),
		minflt:    int64(self.Minflt) + int64(kids.Minflt),
		majflt:    int64(self.Majflt) + int64(kids.Majflt),
	}
}

// since is what was used between an earlier reading and this one.
func (u usage) since(earlier usage) usage {
	return usage{
		runcExecs: u.runcExecs - earlier.runcExecs,
		user:      u.user - earlier.user,
		sys:       u.sys - earlier.sys,
		minflt:    u.minflt - earlier.minflt,
		majflt:    u.majflt - earlier.majflt,
	}
}

// addTo stores the usage in the timings of a request.
func (u usage) addTo(t *proto.Timings) {
	t.RuncExecs = int(u.runcExecs)
	t.CPUUser, t.CPUSys = u.user, u.sys
	t.MinorFaults, t.MajorFaults = u.minflt, u.majflt
}

// withUsage runs a handler that returns a RunResult (run, reload, prepare)
// and adds the usage over its whole run to the result's timings.
func withUsage(handler func() (*proto.RunResult, error)) (*proto.RunResult, error) {
	before := readUsage()
	res, err := handler()
	if res != nil {
		readUsage().since(before).addTo(&res.Timings)
	}
	return res, err
}
