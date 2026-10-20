//go:build linux

package agent

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// bootTimelinePath is written by PID 1 (vm mode) with guest uptimes at
// boot milestones.
const bootTimelinePath = "/run/oar/boot.json"

var bootMu sync.Mutex

// markBoot records the current guest uptime under name.
func markBoot(name string) { markBootAt(name, uptime()) }

// markBootAt records a milestone measured earlier (before /run existed).
func markBootAt(name string, t float64) {
	bootMu.Lock()
	defer bootMu.Unlock()
	m := map[string]float64{}
	if b, err := os.ReadFile(bootTimelinePath); err == nil {
		json.Unmarshal(b, &m)
	}
	m[name] = t
	os.MkdirAll(filepath.Dir(bootTimelinePath), 0o755)
	b, _ := json.Marshal(m)
	os.WriteFile(bootTimelinePath, b, 0o644)
}

// uptime is the time since boot in seconds (CLOCK_BOOTTIME, which needs no
// /proc and so works from the first instruction of init).
func uptime() float64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9
}

func (s *Server) stats() proto.StatsResult {
	res := proto.StatsResult{CacheBytes: map[string]int64{}, CacheFiles: map[string]int64{}}
	for _, area := range []string{"bases", "store", "components", "bundles", "instances"} {
		var bytes, files int64
		root := filepath.Join(s.cfg.Root, area)
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			// do not descend into instance rootfs mounts
			if d.IsDir() && p != root && strings.HasSuffix(p, "/rootfs") && area == "instances" {
				return fs.SkipDir
			}
			if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
				bytes += info.Size()
			}
			files++
			return nil
		})
		res.CacheBytes[area], res.CacheFiles[area] = bytes, files
	}
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		res.Mounts = strings.Count(string(b), "\n")
	}
	s.groups.mu.Lock()
	res.StoreViews = len(s.groups.mounted)
	s.groups.mu.Unlock()
	if list, err := s.loadInstances(); err == nil {
		res.Instances = len(list)
	}
	if entries, err := os.ReadDir("/sys/fs/cgroup/oar"); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				res.Cgroups++
			}
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			v, _ := strconv.ParseInt(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				res.MemTotal = v << 10
			case "MemAvailable:":
				res.MemAvailable = v << 10
			case "Dirty:", "Writeback:":
				res.Dirty += v << 10
			case "Cached:":
				res.Cached = v << 10
			}
		}
		f.Close()
	}
	res.Uptime = uptime()
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		res.Load1, _ = strconv.ParseFloat(strings.Fields(string(b))[0], 64)
	}
	if s.cfg.Mode == "vm" {
		if b, err := os.ReadFile(bootTimelinePath); err == nil {
			json.Unmarshal(b, &res.Boot)
		}
	}
	return res
}
