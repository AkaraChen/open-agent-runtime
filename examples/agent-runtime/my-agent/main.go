// Command my-agent is the example custom binary. It reports the runtime
// environment it observes (tool versions, PATH, OS) and then idles until it
// receives SIGTERM. With MY_AGENT_LISTEN set it also serves that report over
// HTTP (MY_AGENT_REUSEPORT=1 enables SO_REUSEPORT so a new instance can bind
// the same port while the old one drains).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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
	// MY_AGENT_INIT_DELAY simulates a slow-starting application (e.g. loading
	// a model or importing a large framework) before it starts serving.
	if d, err := time.ParseDuration(os.Getenv("MY_AGENT_INIT_DELAY")); err == nil && d > 0 {
		time.Sleep(d)
	}
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
	if v := os.Getenv("VARIANT"); v != "" {
		report["variant"] = v
	}
	b, _ := json.Marshal(report)

	var srv *http.Server
	if addr := os.Getenv("MY_AGENT_LISTEN"); addr != "" {
		lc := net.ListenConfig{}
		if os.Getenv("MY_AGENT_REUSEPORT") == "1" {
			lc.Control = setReusePort
		}
		ln, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			fmt.Printf("my-agent: listen %s: %v\n", addr, err)
			os.Exit(1)
		}
		pid := fmt.Sprint(os.Getpid())
		srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Variant", report["variant"])
			w.Header().Set("X-Pid", pid)
			w.Write(b)
		})}
		go srv.Serve(ln)
		fmt.Printf("my-agent: serving on %s\n", addr)
	}
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
			if srv != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				srv.Shutdown(ctx) // finish in-flight requests
				cancel()
			}
			return
		case t := <-tick.C:
			fmt.Printf("my-agent: alive %s\n", t.UTC().Format(time.RFC3339))
		}
	}
}
