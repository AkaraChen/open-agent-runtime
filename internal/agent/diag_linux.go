//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DIAG: samples kernel stacks of runc processes until stop is closed and
// returns a histogram "comm state frame" -> samples.
func sampleRunc(stop chan struct{}) string {
	counts := map[string]int{}
	total := 0
	for {
		select {
		case <-stop:
			type kv struct {
				k string
				n int
			}
			var l []kv
			for k, n := range counts {
				l = append(l, kv{k, n})
			}
			sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n })
			var b strings.Builder
			fmt.Fprintf(&b, "DIAG %d samples (2ms):", total)
			for i, e := range l {
				if i == 12 {
					break
				}
				fmt.Fprintf(&b, " [%d %s]", e.n, e.k)
			}
			return b.String()
		default:
		}
		total++
		dirs, _ := filepath.Glob("/proc/[0-9]*")
		for _, d := range dirs {
			comm, err := os.ReadFile(d + "/comm")
			if err != nil || !strings.HasPrefix(string(comm), "runc") {
				continue
			}
			stat, _ := os.ReadFile(d + "/stat")
			state := "?"
			if i := strings.LastIndexByte(string(stat), ')'); i > 0 && i+2 < len(stat) {
				state = string(stat[i+2])
			}
			frame := "-"
			if state != "R" {
				st, _ := os.ReadFile(d + "/stack")
				var frames []string
				for _, line := range strings.Split(string(st), "\n") {
					f := strings.Fields(line)
					if len(f) < 2 {
						continue
					}
					name := f[1]
					if i := strings.IndexByte(name, '+'); i > 0 {
						name = name[:i]
					}
					if strings.Contains(name, "schedule") || strings.HasPrefix(name, "io_schedule") || strings.HasPrefix(name, "bit_wait") || strings.HasPrefix(name, "__wait_on") || strings.HasPrefix(name, "out_of_line_wait") {
						continue
					}
					frames = append(frames, name)
					if len(frames) == 3 {
						break
					}
				}
				frame = strings.Join(frames, "<")
			}
			counts[strings.TrimSpace(string(comm))+" "+state+" "+frame]++
		}
		time.Sleep(2 * time.Millisecond)
	}
}
