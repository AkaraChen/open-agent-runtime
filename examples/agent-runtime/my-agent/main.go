// Command my-agent is the example custom binary. It reports the runtime
// environment it observes (tool versions, PATH, OS) and then idles until it
// receives SIGTERM.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func version(cmd string, args ...string) string {
	out, err := exec.Command(cmd, args...).CombinedOutput()
	if err != nil {
		return "error: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

func osRelease() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func main() {
	host, _ := os.Hostname()
	report := map[string]string{
		"arch":     runtime.GOARCH,
		"hostname": host,
		"os":       osRelease(),
		"node":     version("node", "--version"),
		"python":   version("python3", "--version"),
		"git":      version("git", "--version"),
		"PATH":     os.Getenv("PATH"),
	}
	b, _ := json.Marshal(report)
	fmt.Printf("my-agent ready %s\n", b)
	os.WriteFile("/tmp/my-agent.json", b, 0o644)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case s := <-sig:
			fmt.Printf("my-agent: received %v, exiting\n", s)
			return
		case t := <-tick.C:
			fmt.Printf("my-agent: alive %s\n", t.UTC().Format(time.RFC3339))
		}
	}
}
