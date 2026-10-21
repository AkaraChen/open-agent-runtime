package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/proto"
	"github.com/AkaraChen/open-agent-runtime/internal/vm"
)

const (
	port        = 18080
	ingressPort = 18081
)

type bench struct {
	oar      string
	backend  string
	arch     string
	platform string
	home     string
	art      string
	exDir    string
	root     string // native backend cache dir
	vmArgs   []string
	data     string // XDG_DATA_HOME of the CLI runs
	guest    string // guest image directory (-guest-dir)
	quick    bool
	res      *Result
	digests  map[string]string
	configs  []string
	// noIngress skips what needs the ingress proxy (process.ports,
	// prepare --start), so builds from before it can be compared (A/B)
	noIngress bool
}

func runMain(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	backend := fs.String("backend", "native", "backend")
	out := fs.String("out", "results.json", "output file")
	profile := fs.String("profile", "quick", "quick or full")
	oarBin := fs.String("oar", "bin/oar", "oar binary")
	guestDir := fs.String("guest-dir", "", "guest image directory with <arch>/ subdirectories (default: guest/out of this repository)")
	example := fs.String("example", "examples/agent-runtime", "example directory (config templates live here)")
	arch := fs.String("arch", runtime.GOARCH, "guest architecture")
	keep := fs.Bool("keep", false, "keep artifacts and configs (debugging)")
	only := fs.String("only", "", "comma separated phases to run (debugging)")
	noIngress := fs.Bool("no-ingress", false, "skip scenarios that need the ingress proxy (to compare with older builds)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	oarAbs, _ := filepath.Abs(*oarBin)
	exAbs, _ := filepath.Abs(*example)
	b := &bench{
		oar: oarAbs, backend: *backend, arch: *arch, platform: "linux/" + *arch,
		exDir: exAbs, vmArgs: fs.Args(), quick: *profile != "full", guest: *guestDir,
		digests: map[string]string{}, noIngress: *noIngress,
		res: &Result{Backend: *backend, Platform: "linux/" + *arch, Profile: *profile, Started: time.Now()},
	}
	b.res.Host = hostDesc()
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		b.res.Commit = strings.TrimSpace(string(out))
	}
	var err error
	if b.data, err = os.MkdirTemp("", "oar-bench-home-"); err != nil {
		return err
	}
	b.home = filepath.Join(b.data, "oar") // $XDG_DATA_HOME/oar
	// artifacts can be large; keep them next to the example (not in /tmp)
	if b.art, err = os.MkdirTemp(b.exDir, ".bench-artifacts-"); err != nil {
		return err
	}
	if b.backend == "native" {
		b.root = fmt.Sprintf("/var/lib/oar-bench-%d", rand.IntN(1<<30))
	}
	if !*keep {
		defer b.cleanup()
	} else {
		defer logf("kept artifacts in %s, oar state in %s", b.art, b.home)
	}

	start := time.Now()
	steps := []struct {
		name string
		fn   func() error
	}{
		{"env", b.phaseEnv},
		{"build", b.phaseBuild},
		{"vm", b.phaseVM},
		{"warm", b.phaseWarm},
		{"start", b.phaseStart},
		{"runtime", b.phaseRuntime},
		{"density", b.phaseDensity},
		{"switch", b.phaseSwitch},
		{"snapshot", b.phaseSnapshot},
		{"correctness", b.phaseCorrectness},
		{"vm-restart", b.phaseVMRestart},
	}
	for _, s := range steps {
		if *only != "" && !strings.Contains(","+*only+",", ","+s.name+",") {
			continue
		}
		logf("==> %s", s.name)
		if err := s.fn(); err != nil {
			b.res.Notes = append(b.res.Notes, fmt.Sprintf("phase %s failed: %v", s.name, err))
			logf("phase %s failed: %v", s.name, err)
			b.saveVMLogs(*out, s.name)
			if s.name == "build" || s.name == "vm" || s.name == "warm" {
				break
			}
		}
	}
	b.res.Duration = time.Since(start).Seconds()
	return writeJSON(*out, b.res)
}

func logf(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }

func hostDesc() string {
	h, _ := os.Hostname()
	desc := runtime.GOOS + "/" + runtime.GOARCH + " " + h
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				desc += " · " + strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
				break
			}
		}
	} else if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		desc += " · " + strings.TrimSpace(string(out))
	}
	return fmt.Sprintf("%s · %d CPU", desc, runtime.NumCPU())
}

// ---- metric helpers ----

func (b *bench) add(group, key, name, unit string, lower bool, samples ...float64) *Metric {
	m := Metric{Group: group, Key: key, Name: name, Unit: unit, Samples: samples, Status: "ok", LowerIsBetter: lower}
	summarize(&m)
	b.res.Metrics = append(b.res.Metrics, m)
	return &b.res.Metrics[len(b.res.Metrics)-1]
}

func (b *bench) na(group, key, name, note string) {
	b.res.Metrics = append(b.res.Metrics, Metric{Group: group, Key: key, Name: name, Status: "na", Note: note})
}

func (b *bench) fail(group, key, name, note string) {
	b.res.Metrics = append(b.res.Metrics, Metric{Group: group, Key: key, Name: name, Status: "fail", Note: note})
}

func boolVal(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// ---- command helpers ----

func (b *bench) env() []string {
	// XDG_DATA_HOME for the CLI; OAR_HOME for older binaries in an A/B
	return append(os.Environ(), "XDG_DATA_HOME="+b.data, "OAR_HOME="+b.home)
}

// guestDir is the guest image directory passed to vm start.
func (b *bench) guestDir() string {
	if b.guest != "" {
		return b.guest
	}
	return filepath.Join(filepath.Dir(filepath.Dir(b.exDir)), "guest", "out")
}

// oarCmd runs the CLI and returns stdout and the wall time.
func (b *bench) oarCmd(args ...string) ([]byte, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	full := append([]string{"--artifacts", b.art}, args...)
	cmd := exec.CommandContext(ctx, b.oar, full...)
	cmd.Env = b.env()
	cmd.Dir = b.exDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	t := time.Now()
	err := cmd.Run()
	d := time.Since(t)
	if err != nil {
		return stdout.Bytes(), d, fmt.Errorf("oar %s: %w: %s", strings.Join(args, " "), err, lastLines(stderr.String(), 6))
	}
	return stdout.Bytes(), d, nil
}

func (b *bench) oarJSON(v any, args ...string) (time.Duration, error) {
	out, d, err := b.oarCmd(append([]string{"--json"}, args...)...)
	if err != nil {
		return d, err
	}
	if v != nil {
		if err := json.Unmarshal(out, v); err != nil {
			return d, fmt.Errorf("decode %s: %w: %s", strings.Join(args, " "), err, lastLines(string(out), 3))
		}
	}
	return d, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func (b *bench) client() *proto.Client { return vm.Client(vm.Dir(b.home, "default")) }

func (b *bench) stats() (proto.StatsResult, error) { return b.statsOpt(false) }

func (b *bench) statsOpt(drop bool) (proto.StatsResult, error) {
	return b.statsArgs(proto.StatsArgs{DropCaches: drop})
}

func (b *bench) statsArgs(a proto.StatsArgs) (proto.StatsResult, error) {
	var st proto.StatsResult
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err := b.client().Call(ctx, proto.OpStats, a, &st)
	return st, err
}

// exec runs a command in a container and returns its stdout.
func (b *bench) exec(name string, args ...string) (string, error) {
	var res proto.ExecResult
	if _, err := b.oarJSON(&res, append([]string{"exec", "--name", name, "--"}, args...)...); err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("exit %d: %s", res.ExitCode, lastLines(res.Stderr, 4))
	}
	return res.Stdout, nil
}

// ---- configs ----

type cfgOpts struct {
	extra      []string // extra components
	drop       string   // component to drop
	env        map[string]string
	ready      string // log readiness substring
	readyTO    string
	ingress    bool     // serve port 18081 through the ingress proxy
	snapshot   bool     // process.snapshot
	components []string // explicit component order (shuffle tests)
}

func (b *bench) writeConfig(name string, o cfgOpts) string {
	comps := o.components
	if comps == nil {
		for _, c := range []string{"nix:nodejs_22", "nix:python313", "nix:git", "binary:my-agent"} {
			if c != o.drop {
				comps = append(comps, c)
			}
		}
		comps = append(comps, o.extra...)
	}
	var sb strings.Builder
	sb.WriteString("runtime:\n  base: debian:trixie-slim\n  platform: " + b.platform + "\n\ncomponents:\n")
	for _, c := range comps {
		sb.WriteString("  - " + c + "\n")
	}
	sb.WriteString(`
sources:
  my-agent:
    path: "dist/my-agent-{{arch}}"
    target: /opt/my-agent
    build: [go, build, -trimpath, -buildvcs=false, "-ldflags=-s -w", -o, "dist/my-agent-{{arch}}", ./my-agent]
    build_env: {GOOS: linux, GOARCH: "{{arch}}", CGO_ENABLED: "0"}

process:
  command: ["/opt/my-agent"]
`)
	if len(o.env) > 0 {
		sb.WriteString("  env:\n")
		keys := make([]string, 0, len(o.env))
		for k := range o.env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "    %s: %q\n", k, o.env[k])
		}
	}
	if o.ingress {
		fmt.Fprintf(&sb, "  ports:\n    - {name: http, port: %d, env: MY_AGENT_PORT}\n", ingressPort)
	}
	if o.snapshot {
		sb.WriteString("  snapshot: true\n")
	}
	if o.ready != "" {
		to := o.readyTO
		if to == "" {
			to = "20s"
		}
		fmt.Fprintf(&sb, "  ready:\n    log: %q\n    timeout: %s\n", o.ready, to)
	}
	path := filepath.Join(b.exDir, ".bench-"+name+".yaml")
	os.WriteFile(path, []byte(sb.String()), 0o644)
	b.configs = append(b.configs, path)
	return path
}

func (b *bench) writeProbeConfig() string { return b.writeProbeConfigFor("probe", port) }

func (b *bench) writeProbeConfigFor(name string, target int) string {
	content := fmt.Sprintf(`runtime:
  base: debian:trixie-slim
  platform: %s
components: [nix:python313, file:probe]
sources:
  probe:
    target: /opt/probe.py
    content: |
      import json, signal, time, urllib.request
      url, stop = "http://127.0.0.1:%d/", False
      def term(*_):
          global stop
          stop = True
      signal.signal(signal.SIGTERM, term)
      ok = fail = 0
      last_ok = prev = None
      max_gap = 0.0
      changes = 0
      t0 = time.time()
      while not stop:
          t = time.time()
          try:
              with urllib.request.urlopen(url, timeout=0.5) as r:
                  v = r.headers.get("X-Variant"); r.read()
              ok += 1
              if last_ok is not None and prev is not None:
                  max_gap = max(max_gap, t - last_ok)
              last_ok = t
              if v != prev:
                  changes += 1; prev = v
          except Exception:
              fail += 1
          time.sleep(0.005)
      print("probe-summary " + json.dumps({"ok": ok, "failed": fail, "maxGapMs": round(max_gap * 1000, 1),
            "changes": changes, "seconds": round(time.time() - t0, 1)}), flush=True)
process:
  command: ["python3", "-u", "/opt/probe.py"]
`, b.platform, target)
	path := filepath.Join(b.exDir, ".bench-"+name+".yaml")
	os.WriteFile(path, []byte(content), 0o644)
	b.configs = append(b.configs, path)
	return path
}

type buildOut struct {
	Platforms map[string]*manifest.PlatformLock `json:"platforms"`
	Stats     struct {
		Total         float64 `json:"total"`
		Base          float64 `json:"base"`
		Eval          float64 `json:"eval"`
		Fetch         float64 `json:"fetch"`
		Components    float64 `json:"components"`
		EvaluatedAttr int     `json:"evaluatedAttrs"`
		FetchedPaths  int     `json:"fetchedPaths"`
		FetchedBytes  int64   `json:"fetchedBytes"`
		ClosurePaths  int     `json:"closurePaths"`
		ClosureBytes  int64   `json:"closureBytes"`
	} `json:"stats"`
}

// buildSplit describes where a build's wall time went.
func buildSplit(o *buildOut, wall time.Duration) string {
	st := o.Stats
	return fmt.Sprintf("构建器 %.0f ms（基础镜像 %.0f、求值 %.0f、下载 %.0f、组件 %.0f），其余（CLI 进程、配置、锁文件）%.0f ms",
		st.Total/1e6, st.Base/1e6, st.Eval/1e6, st.Fetch/1e6, st.Components/1e6, ms(wall)-st.Total/1e6)
}

func (b *bench) build(name, cfg string) (*buildOut, time.Duration, error) {
	var out buildOut
	d, err := b.oarJSON(&out, "-f", cfg, "build", "--platform", b.platform)
	if err != nil {
		return nil, d, err
	}
	pl := out.Platforms[b.platform]
	if pl == nil {
		return nil, d, fmt.Errorf("no %s bundle for %s", b.platform, name)
	}
	b.digests[name] = pl.BundleDigest
	return &out, d, nil
}

func (b *bench) cleanup() {
	b.oarCmd("vm", "stop")
	for _, c := range b.configs {
		os.Remove(c)
	}
	os.RemoveAll(b.art)
	os.RemoveAll(b.data)
	if b.root != "" {
		exec.Command("sudo", "rm", "-rf", b.root).Run()
	}
}

// ---- phase 0: environment calibration ----

// phaseEnv measures the host itself so that runner-to-runner variance can
// be told apart from changes in oar.
func (b *bench) phaseEnv() error {
	const g = "0-env"
	var fork []float64
	for i := 0; i < 50; i++ {
		t := time.Now()
		if err := exec.Command("true").Run(); err != nil {
			return err
		}
		fork = append(fork, ms(time.Since(t)))
	}
	b.add(g, "fork", "宿主机 fork+exec 延迟（true）", "ms", true, fork...)
	var cpu []float64
	buf := make([]byte, 64<<20)
	for i := 0; i < 3; i++ {
		t := time.Now()
		sha256.Sum256(buf)
		cpu = append(cpu, ms(time.Since(t)))
	}
	b.add(g, "cpu", "宿主机单核 CPU（sha256 64 MiB）", "ms", true, cpu...)
	f, err := os.CreateTemp(b.art, "disk-")
	if err == nil {
		t := time.Now()
		for i := 0; i < 4; i++ {
			f.Write(buf)
		}
		f.Sync()
		d := time.Since(t)
		f.Close()
		os.Remove(f.Name())
		b.add(g, "disk", "宿主机磁盘顺序写（256 MiB + fsync）", "MiB/s", false, 256/d.Seconds())
	}
	return nil
}

// ---- phase 1: build ----

func (b *bench) phaseBuild() error {
	const g = "1-build"
	a := b.writeConfig("a", cfgOpts{env: map[string]string{"VARIANT": "a", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "1"}, ready: "my-agent: serving on"})
	out, wall, err := b.build("a", a)
	if err != nil {
		return err
	}
	s := out.Stats
	b.add(g, "cold.total", "全量构建（空产物目录）", "s", true, wall.Seconds())
	b.add(g, "cold.base", "　其中：基础镜像拉取与展平", "s", true, s.Base/1e9)
	b.add(g, "cold.eval", "　其中：Nix 求值", "s", true, s.Eval/1e9).Note = "nix 自身的 tarball/eval 缓存可能已预热"
	b.add(g, "cold.fetch", "　其中：闭包下载与 NAR→tar", "s", true, s.Fetch/1e9)
	b.add(g, "cold.components", "　其中：组件构建与导入", "s", true, s.Components/1e9)
	b.add(g, "cold.fetched", "下载的 store path / 字节", "MiB", true, float64(s.FetchedBytes)/(1<<20)).Note = fmt.Sprintf("%d 个 store path", s.FetchedPaths)
	b.add(g, "closure.paths", "闭包 store path 数", "个", true, float64(s.ClosurePaths))
	b.add(g, "closure.size", "闭包大小（tar）", "MiB", true, float64(s.ClosureBytes)/(1<<20))

	// no-op rebuild
	on, wall, err := b.build("a", a)
	if err == nil {
		b.add(g, "noop", "无变化重建", "ms", true, ms(wall)).Note = buildSplit(on, wall)
	}
	// incremental: drop a component; three combos (they differ only in an
	// env var) so one noisy sample does not decide
	var removes []float64
	var splits []string
	var o2 *buildOut
	for _, name := range []string{"nogit", "nogit-2", "nogit-3"} {
		cfg := b.writeConfig(name, cfgOpts{drop: "nix:git", env: map[string]string{"VARIANT": name}})
		o, wall, err := b.build(name, cfg)
		if err != nil {
			break
		}
		if o2 == nil {
			o2 = o
		}
		removes = append(removes, ms(wall))
		splits = append(splits, buildSplit(o, wall))
	}
	if o2 != nil {
		b.add(g, "incr.remove", "增量构建：删除一个组件（nix:git，3 个组合）", "ms", true, removes...).Note =
			fmt.Sprintf("第一个：下载 %d 个 path，求值 %d 个属性；%s", o2.Stats.FetchedPaths, o2.Stats.EvaluatedAttr, strings.Join(splits, " | "))
	}
	// add a package that was never evaluated
	hello := b.writeConfig("hello", cfgOpts{extra: []string{"nix:hello"}, env: map[string]string{"VARIANT": "hello"}})
	o3, wall, err := b.build("hello", hello)
	if err == nil {
		b.add(g, "incr.addnew", "增量构建：新增未求值过的包（nix:hello）", "s", true, wall.Seconds()).Note =
			fmt.Sprintf("求值 %.1fs，下载 %d 个 path / %.1f MiB", o3.Stats.Eval/1e9, o3.Stats.FetchedPaths, float64(o3.Stats.FetchedBytes)/(1<<20))
	}
	// dedup: B adds jq to A
	bcfg := b.writeConfig("b", cfgOpts{extra: []string{"nix:jq"}, env: map[string]string{"VARIANT": "b", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "1"}, ready: "my-agent: serving on"})
	o4, _, err := b.build("b", bcfg)
	if err == nil && o4.Stats.ClosureBytes > 0 {
		b.add(g, "dedup", "去重率：新增 jq 的组合实际下载 / 闭包大小", "%", true,
			100*float64(o4.Stats.FetchedBytes)/float64(o4.Stats.ClosureBytes)).Note =
			fmt.Sprintf("下载 %.1f MiB / 闭包 %.1f MiB", float64(o4.Stats.FetchedBytes)/(1<<20), float64(o4.Stats.ClosureBytes)/(1<<20))
	}
	// artifact sizes
	sizes := map[string]float64{}
	filepath.WalkDir(b.art, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(b.art, p)
			if info, err := d.Info(); err == nil {
				sizes[strings.SplitN(rel, string(filepath.Separator), 2)[0]] += float64(info.Size())
			}
		}
		return nil
	})
	b.add(g, "art.total", "产物目录总大小（含所有已构建组合）", "MiB", true, (sizes["bases"]+sizes["store"]+sizes["components"]+sizes["manifests"])/(1<<20)).Note =
		fmt.Sprintf("bases %.0f MiB, store %.0f MiB, components %.1f MiB", sizes["bases"]/(1<<20), sizes["store"]/(1<<20), sizes["components"]/(1<<20))
	if pl := out.Platforms[b.platform]; pl != nil {
		var ps []float64
		for _, sp := range pl.StorePaths {
			ps = append(ps, float64(sp.Size)/(1<<20))
		}
		m := b.add(g, "art.pathsize", "单个 store path 大小分布", "MiB", true, ps...)
		m.Samples = nil
	}

	// remaining configs used later
	plain := map[string]string{"VARIANT": "a", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "0"}
	cfgs := map[string]cfgOpts{
		"a-plain": {env: plain, ready: "my-agent: serving on"},
		"b-plain": {extra: []string{"nix:jq"}, env: map[string]string{"VARIANT": "b", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "0"}, ready: "my-agent: serving on"},
		"slow-a":  {env: map[string]string{"VARIANT": "slow-a", "MY_AGENT_INIT_DELAY": "3s", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "1"}, ready: "my-agent: serving on"},
		"slow-b":  {env: map[string]string{"VARIANT": "slow-b", "MY_AGENT_INIT_DELAY": "3s", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "1"}, ready: "my-agent: serving on"},
		"stop-a":  {env: map[string]string{"VARIANT": "stop-a", "MY_AGENT_SHUTDOWN_DELAY": "1s", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "1"}, ready: "my-agent: serving on"},
		"stop-b":  {env: map[string]string{"VARIANT": "stop-b", "MY_AGENT_SHUTDOWN_DELAY": "1s", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "1"}, ready: "my-agent: serving on"},
		"broken":  {env: map[string]string{"VARIANT": "broken", "MY_AGENT_LISTEN": fmt.Sprintf("127.0.0.1:%d", port), "MY_AGENT_REUSEPORT": "0"}, ready: "never printed", readyTO: "2s"},
		"idle":    {env: map[string]string{"VARIANT": "idle"}},
		"ing-a":   {env: map[string]string{"VARIANT": "ing-a"}, ingress: true},
		"ing-b":   {extra: []string{"nix:jq"}, env: map[string]string{"VARIANT": "ing-b"}, ingress: true},
		"slowi-a": {env: map[string]string{"VARIANT": "slowi-a", "MY_AGENT_INIT_DELAY": "3s"}, ingress: true},
		"slowi-b": {env: map[string]string{"VARIANT": "slowi-b", "MY_AGENT_INIT_DELAY": "3s"}, ingress: true},
		"snap-a":  {env: map[string]string{"VARIANT": "snap-a", "MY_AGENT_INIT_DELAY": "3s"}, ingress: true, snapshot: true},
		"snap-b":  {env: map[string]string{"VARIANT": "snap-b", "MY_AGENT_INIT_DELAY": "3s"}, ingress: true, snapshot: true},
	}
	for name, o := range cfgs {
		if o.ingress && b.noIngress {
			continue
		}
		if _, _, err := b.build(name, b.writeConfig(name, o)); err != nil {
			return err
		}
	}
	if _, _, err := b.build("probe", b.writeProbeConfig()); err != nil {
		return err
	}
	if b.noIngress {
		return nil
	}
	if _, _, err := b.build("probe-ing", b.writeProbeConfigFor("probe-ing", ingressPort)); err != nil {
		return err
	}
	return nil
}

// ---- phase 3: VM ----

func (b *bench) phaseVM() error {
	const g = "3-vm"
	args := []string{"vm", "start", "--backend", b.backend, "--arch", b.arch, "--guest-dir", filepath.Join(b.guestDir(), b.arch)}
	if b.root != "" {
		args = append(args, "--root", b.root)
	}
	args = append(args, b.vmArgs...)
	_, wall, err := b.oarCmd(args...)
	if err != nil {
		return err
	}
	b.add(g, "boot.cold", "虚拟机冷启动（含首次格式化数据盘）", "ms", true, ms(wall)).Note = "CLI 墙钟：vm start 到 agent 可用"
	if vs, err := vm.ReadState(vm.Dir(b.home, "default")); err == nil && vs.Phases != nil {
		ready := ms(vs.Phases["socketReady"])
		b.add(g, "boot.cli", "　其中：CLI 拉起 supervisor 进程（含 daemonize）", "ms", true, math.Max(0, ms(wall)-ready))
		b.add(g, "boot.machine", "　其中：后端创建并启动虚拟机（Boot 返回）", "ms", true, ms(vs.Phases["machineStarted"]))
		b.add(g, "boot.reach", "　其中：虚拟机启动后到 agent 首次可达", "ms", true, ms(vs.Phases["agentReachable"]-vs.Phases["machineStarted"]))
		// host-side arrival of guest console markers
		if t, ok := vs.Phases["consoleInit"]; ok {
			b.add(g, "boot.toinit", "　宿主机看到 guest init 启动（VMM + 固件 + 内核解压 + 内核启动）", "ms", true, ms(t))
		}
		if t, ok := vs.Phases["consoleAgent"]; ok {
			b.add(g, "boot.detect", "　guest agent 开始监听到宿主机连上（探测延迟）", "ms", true, math.Max(0, ms(vs.Phases["agentReachable"]-t)))
		}
	}
	st, err := b.stats()
	if err != nil {
		return err
	}
	if t, ok := st.Boot["agentReady"]; ok {
		b.add(g, "boot.guest", "　其中：guest 内核 + init 到 agent 就绪", "ms", true, t*1000)
		b.add(g, "boot.vmm", "　其中：VMM/宿主侧开销（总时间 − guest 时间）", "ms", true, math.Max(0, ms(wall)-t*1000))
		if k, ok := st.Boot["kernel"]; ok {
			b.add(g, "boot.kernel", "　其中：guest 内核启动（内核时钟开始到运行 init，含解压 initramfs）", "ms", true, k*1000)
			if sr, ok := st.Boot["switchRoot"]; ok {
				b.add(g, "boot.switchroot", "　其中：switch_root（离开 initramfs）", "ms", true, (sr-k)*1000)
			}
		}
		if fs, ok := st.Boot["formatStart"]; ok {
			b.add(g, "boot.format", "　其中：格式化数据盘", "ms", true, (st.Boot["dataDisk"]-fs)*1000)
		}
	} else {
		b.na(g, "boot.guest", "guest 启动阶段拆分", "native 后端没有 guest 内核")
	}
	// control RTT
	var rtt []float64
	for i := 0; i < 50; i++ {
		t := time.Now()
		var info proto.InfoResult
		if err := b.client().Call(context.Background(), proto.OpInfo, nil, &info); err != nil {
			return err
		}
		rtt = append(rtt, ms(time.Since(t)))
	}
	b.add(g, "rtt", "控制通道往返（info，经宿主 socket → guest agent）", "ms", true, rtt...)
	var cli []float64
	for i := 0; i < 5; i++ {
		_, d, err := b.oarCmd("vm", "status", "--json")
		if err != nil {
			return err
		}
		cli = append(cli, ms(d))
	}
	b.add(g, "rtt.cli", "CLI 单次命令开销（vm status）", "ms", true, cli...)
	// idle resources
	rss, cpu, note := b.idleResources(10 * time.Second)
	if rss > 0 {
		b.add(g, "idle.rss", "空闲时宿主机内存占用（supervisor + VMM 进程树 RSS）", "MiB", true, rss/(1<<20)).Note = note
		b.add(g, "idle.cpu", "空闲时宿主机 CPU 占用（10 秒平均）", "%", true, cpu)
	} else {
		b.na(g, "idle.rss", "空闲时宿主机资源占用", note)
	}
	b.add(g, "guest.mem", "guest 可用内存 / 总内存", "MiB", false, float64(st.MemAvailable)/(1<<20)).Note = fmt.Sprintf("总内存 %.0f MiB", float64(st.MemTotal)/(1<<20))
	return nil
}

// idleResources samples RSS and CPU time of the supervisor process tree.
func (b *bench) idleResources(window time.Duration) (float64, float64, string) {
	state, err := vm.ReadState(vm.Dir(b.home, "default"))
	if err != nil {
		return 0, 0, err.Error()
	}
	if b.backend == "native" {
		return 0, 0, "native 后端没有 VM；agent 运行在 supervisor 进程内"
	}
	sample := func() (float64, float64) {
		out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=,time=,comm=").Output()
		if err != nil {
			return 0, 0
		}
		type p struct {
			ppid int
			rss  float64
			cpu  float64
		}
		procs := map[int]p{}
		in := map[int]bool{state.Pid: true}
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(l)
			if len(f) < 4 {
				continue
			}
			pid, _ := strconv.Atoi(f[0])
			ppid, _ := strconv.Atoi(f[1])
			rss, _ := strconv.ParseFloat(f[2], 64)
			procs[pid] = p{ppid, rss * 1024, parseCPUTime(f[3])}
			// Virtualization.framework runs the VM in an XPC service that is
			// not a child of the supervisor
			if len(f) > 4 && strings.Contains(strings.Join(f[4:], " "), "com.apple.Virtualization.VirtualMachine") {
				in[pid] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for pid, pr := range procs {
				if !in[pid] && in[pr.ppid] {
					in[pid], changed = true, true
				}
			}
		}
		var rss, cpu float64
		for pid := range in {
			rss += procs[pid].rss
			cpu += procs[pid].cpu
		}
		return rss, cpu
	}
	_, c0 := sample()
	time.Sleep(window)
	rss, c1 := sample()
	note := "ps 采样"
	if b.backend == "vz" {
		note = "ps 采样，含 com.apple.Virtualization.VirtualMachine XPC 进程"
	}
	return rss, 100 * (c1 - c0) / window.Seconds(), note
}

// parseCPUTime parses ps TIME ([[dd-]hh:]mm:ss[.cc]).
func parseCPUTime(s string) float64 {
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseFloat(d, 64)
		s = rest
	}
	parts := strings.Split(s, ":")
	total := 0.0
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		total = total*60 + v
	}
	return days*86400 + total
}

// ---- phase 2: warm ----

type warmOut struct {
	Total      int     `json:"total"`
	Uploaded   int     `json:"uploaded"`
	Bytes      int64   `json:"bytes"`
	Files      int64   `json:"files"`
	Duration   float64 `json:"duration"`
	GuestTime  float64 `json:"guestTime"`
	Status     float64 `json:"status"`
	Precompose float64 `json:"precompose"`
	Upload     float64 `json:"upload"`
	Wire       int64   `json:"wire"`
	Compressed bool    `json:"compressed"`
	View       float64 `json:"view"`
	Flush      float64 `json:"flush"`
}

func (b *bench) phaseWarm() error {
	const g = "2-warm"
	before, _ := b.stats()
	var w warmOut
	wall, err := b.oarJSON(&w, "--bundle", b.digests["a"], "warm")
	if err != nil {
		return err
	}
	b.add(g, "cold", "冷预热完整组合", "s", true, wall.Seconds()).Note = fmt.Sprintf("%d 个产物，%.0f MiB，%d 个文件", w.Uploaded, float64(w.Bytes)/(1<<20), w.Files)
	if w.Compressed && w.Wire > 0 {
		b.add(g, "cold.wire", "　实际传输（zstd 压缩，含首次压缩）", "MiB", true, float64(w.Wire)/(1<<20)).Note =
			fmt.Sprintf("压缩比 %.2f", float64(w.Bytes)/float64(w.Wire))
	}
	if w.Upload > 0 {
		b.add(g, "cold.upload", "　其中：上传并解包（4 路并发）", "s", true, w.Upload/1e9)
		b.add(g, "cold.view", "　其中：等待 store 视图完成", "ms", true, w.View/1e6)
		b.add(g, "cold.flush", "　其中：写回缓存盘", "ms", true, w.Flush/1e6)
	}
	b.add(g, "cold.mbps", "冷预热吞吐", "MiB/s", false, float64(w.Bytes)/(1<<20)/wall.Seconds())
	b.add(g, "cold.fps", "冷预热文件处理速率", "文件/s", false, float64(w.Files)/wall.Seconds())
	var wb warmOut
	wall, err = b.oarJSON(&wb, "--bundle", b.digests["b"], "warm")
	if err == nil {
		b.add(g, "incr", "增量预热（只多 nix:jq 的组合）", "ms", true, ms(wall)).Note = fmt.Sprintf("上传 %d 个产物，%.2f MiB；CLI 内：查询缺失 %.1f ms，guest 解包合计 %.1f ms，precompose %.1f ms（视图 %.1f ms，写回 %.1f ms），CLI 总计 %.1f ms",
			wb.Uploaded, float64(wb.Bytes)/(1<<20), wb.Status/1e6, wb.GuestTime/1e6, wb.Precompose/1e6, wb.View/1e6, wb.Flush/1e6, wb.Duration/1e6)
	}
	for _, name := range []string{"a-plain", "b-plain", "slow-a", "slow-b", "stop-a", "stop-b", "broken", "idle", "probe", "nogit", "hello", "ing-a", "ing-b", "slowi-a", "slowi-b", "probe-ing", "snap-a", "snap-b"} {
		if b.digests[name] == "" {
			continue // not built (-no-ingress)
		}
		if _, err := b.oarJSON(nil, "--bundle", b.digests[name], "warm"); err != nil {
			return err
		}
	}
	after, err := b.stats()
	if err != nil {
		return err
	}
	var used float64
	for _, k := range []string{"bases", "store", "components"} {
		used += float64(after.CacheBytes[k] - before.CacheBytes[k])
	}
	b.add(g, "guest.size", "guest 缓存占用（全部已预热组合）", "MiB", true, used/(1<<20)).Note =
		fmt.Sprintf("store %.0f MiB / %d 文件，bases %.0f MiB", float64(after.CacheBytes["store"])/(1<<20), after.CacheFiles["store"], float64(after.CacheBytes["bases"])/(1<<20))
	return nil
}

// ---- phase 4: start ----

type runOut struct {
	Result struct {
		Container proto.Container `json:"container"`
		Cached    bool            `json:"cached"`
		Timings   struct {
			Compose, StoreView, Instantiate, Create, Start, Ready, StopOld, Downtime, Drain, Snapshot, Restore, Total float64
		} `json:"timings"`
		Replaced      string `json:"replaced"`
		Restored      bool   `json:"restored"`
		SnapshotBytes int64  `json:"snapshotBytes"`
		SnapshotNote  string `json:"snapshotNote"`
	} `json:"result"`
	Total float64 `json:"total"`
}

func (b *bench) run(name, bundle string) (*runOut, time.Duration, error) {
	var r runOut
	d, err := b.oarJSON(&r, "--bundle", bundle, "run", "--name", name)
	return &r, d, err
}

// rm stops and deletes a container, forcing deletion if a graceful stop
// did not finish (slow backends), so later phases start clean.
func (b *bench) rm(name string) {
	b.oarCmd("stop", "--name", name, "--rm", "--timeout", "5s")
	var list proto.ListResult
	if _, err := b.oarJSON(&list, "ps"); err == nil {
		for _, c := range list.Containers {
			if c.Name == name {
				b.oarCmd("rm", "--name", name, "--force")
			}
		}
	}
}

// joinMs formats samples as "a / b / c".
func joinMs(v []float64) string {
	var parts []string
	for _, x := range v {
		parts = append(parts, fmt.Sprintf("%.1f", x))
	}
	return strings.Join(parts, " / ")
}

func (b *bench) phaseStart() error {
	const g = "4-start"
	pre, err := b.stats()
	if err == nil {
		b.add(g, "dirty", "首次启动前待写回的数据（Dirty + Writeback）", "MiB", true, float64(pre.Dirty+pre.Writeback)/(1<<20))
	}
	first, wall, err := b.run("s0", b.digests["idle"])
	if err != nil {
		return err
	}
	t := first.Result.Timings
	b.add(g, "first.total", "首次启动该组合（端到端，CLI，紧接预热之后）", "ms", true, ms(wall)).Note =
		fmt.Sprintf("compose %.1f ms（其中挂载 store 视图 %.1f ms，cached=%v），create %.0f ms；启动前 guest 脏页 %.0f MiB、写回中 %.0f MiB，挂载 %d 个，视图 %d 个",
			t.Compose/1e6, t.StoreView/1e6, first.Result.Cached, t.Create/1e6,
			float64(pre.Dirty)/(1<<20), float64(pre.Writeback)/(1<<20), pre.Mounts, pre.StoreViews)
	// the same for combos that were warmed but never started, after the
	// guest wrote its dirty pages back: separates first-start cost from
	// the write-back of the preceding warm
	var settled, sCompose, sView, sCreate []float64
	for _, name := range []string{"nogit", "hello", "b-plain"} {
		if _, err := b.statsArgs(proto.StatsArgs{Sync: true}); err != nil {
			return err
		}
		r, wall, err := b.run("f-"+name, b.digests[name])
		if err != nil {
			return err
		}
		t := r.Result.Timings
		settled = append(settled, ms(wall))
		sCompose = append(sCompose, t.Compose/1e6)
		sView = append(sView, t.StoreView/1e6)
		sCreate = append(sCreate, t.Create/1e6)
		b.rm("f-" + name)
	}
	b.add(g, "first.settled", "首次启动该组合（脏页已写回，3 个组合）", "ms", true, settled...).Note =
		fmt.Sprintf("compose %s ms（其中挂载 store 视图 %s ms），create %s ms", joinMs(sCompose), joinMs(sView), joinMs(sCreate))
	mounts, _ := b.exec("s0", "sh", "-c", "wc -l < /proc/self/mountinfo")
	if n, err := strconv.Atoi(strings.TrimSpace(mounts)); err == nil {
		b.add(g, "mounts", "容器内挂载点数量", "个", true, float64(n))
	}
	b.rm("s0")

	n := 10
	if !b.quick {
		n = 30
	}
	var e2e, compose, inst, create, start, total []float64
	for i := 0; i < n; i++ {
		r, wall, err := b.run("s", b.digests["idle"])
		if err != nil {
			return err
		}
		t := r.Result.Timings
		e2e = append(e2e, ms(wall))
		compose = append(compose, t.Compose/1e6)
		inst = append(inst, t.Instantiate/1e6)
		create = append(create, t.Create/1e6)
		start = append(start, t.Start/1e6)
		total = append(total, t.Total/1e6)
		b.rm("s")
	}
	b.add(g, "e2e", "端到端启动（热，CLI 墙钟）", "ms", true, e2e...)
	b.add(g, "guest.total", "guest 内启动总耗时", "ms", true, total...)
	b.add(g, "compose", "　组合（模板命中）", "ms", true, compose...)
	b.add(g, "instantiate", "　实例化（overlay + config.json）", "ms", true, inst...)
	b.add(g, "create", "　runc create", "ms", true, create...)
	b.add(g, "start", "　runc start", "ms", true, start...)
	// readiness for a service with a readiness check
	var ready []float64
	for i := 0; i < 5; i++ {
		r, _, err := b.run("svc", b.digests["a-plain"])
		if err != nil {
			return err
		}
		ready = append(ready, r.Result.Timings.Ready/1e6)
		b.rm("svc")
	}
	b.add(g, "ready", "就绪等待（my-agent 启动到监听）", "ms", true, ready...)
	var cmp []float64
	for i := 0; i < 10; i++ {
		_, d, err := b.oarCmd("--bundle", b.digests["idle"], "compose", "--json")
		if err != nil {
			return err
		}
		cmp = append(cmp, ms(d))
	}
	b.add(g, "compose.cli", "warm compose（CLI 端到端，验收标准 <1s）", "ms", true, cmp...)
	return nil
}

// ---- phase 6: runtime overhead ----

const workload = `
import json, os, time, subprocess, shutil, py_compile, glob
res = {}
def t(name, fn):
    s = time.time(); fn(); res[name] = round((time.time() - s) * 1000, 1)
base = "/root/bench-fs"
os.makedirs(base, exist_ok=True)
N = 5000
t("fs.create", lambda: [open(f"{base}/f{i}", "w").write("x") for i in range(N)])
t("fs.stat", lambda: [os.stat(f"{base}/f{i}") for i in range(N)])
t("fs.chmod", lambda: [os.chmod(f"{base}/f{i}", 0o600) for i in range(N)])
t("fs.unlink", lambda: [os.unlink(f"{base}/f{i}") for i in range(N)])
def write_big():
    with open(f"{base}/big", "wb") as f:
        for _ in range(256): f.write(os.urandom(1 << 20))
        f.flush(); os.fsync(f.fileno())
t("fs.write256m", write_big)
os.unlink(f"{base}/big")
py = [p for p in glob.glob("/nix/store/*-python3-3.13*") if os.path.isdir(p)][0]
def read_tree():
    n = 0
    for root, _, files in os.walk(py):
        for fn in files:
            p = os.path.join(root, fn)
            if os.path.isfile(p) and not os.path.islink(p):
                with open(p, "rb") as f: n += len(f.read())
    res["read.bytes"] = n
t("store.readtree", read_tree)
def compile_all():
    out = "/tmp/pyc"
    src = os.path.join(py, "lib/python3.13/json")
    for root, _, files in os.walk(os.path.join(py, "lib/python3.13")):
        for fn in files:
            if fn.endswith(".py"):
                try: py_compile.compile(os.path.join(root, fn), cfile=out + "/" + fn + "c", doraise=False)
                except Exception: pass
t("cpu.compileall", compile_all)
def git_work():
    d = "/root/repo"; shutil.rmtree(d, ignore_errors=True); os.makedirs(d)
    for i in range(1000):
        with open(f"{d}/file{i}.txt", "w") as f: f.write(f"line {i}\n" * 20)
    run = lambda *a: subprocess.run(a, cwd=d, check=True, capture_output=True)
    run("git", "init", "-q"); run("git", "-c", "user.email=a@b", "-c", "user.name=b", "add", ".")
    run("git", "-c", "user.email=a@b", "-c", "user.name=b", "commit", "-qm", "init")
    subprocess.run(["git", "clone", "-q", d, d + "-clone"], check=True, capture_output=True)
t("git.commit_clone", git_work)
def node_start():
    for _ in range(5): subprocess.run(["node", "-e", "1"], check=True)
t("node.start5", node_start)
def py_start():
    for _ in range(5): subprocess.run(["python3", "-c", "import json, ssl, sqlite3"], check=True)
t("python.start5", py_start)
import socket
lat = []
for _ in range(3):
    s = time.time()
    try:
        socket.create_connection(("1.1.1.1", 443), timeout=3).close(); lat.append((time.time() - s) * 1000)
    except Exception: pass
res["net.connect"] = round(sorted(lat)[len(lat)//2], 1) if lat else -1
s = time.time()
try:
    socket.getaddrinfo("cache.nixos.org", 443); res["net.dns"] = round((time.time() - s) * 1000, 1)
except Exception: res["net.dns"] = -1
print(json.dumps(res))
`

func (b *bench) phaseRuntime() error {
	const g = "6-runtime"
	// pending writeback competes with the workload on slow disks
	if st, err := b.stats(); err == nil {
		b.add(g, "dirty", "开始时待写回的数据（Dirty + Writeback）", "MiB", true, float64(st.Dirty+st.Writeback)/(1<<20))
	}
	if _, _, err := b.run("work", b.digests["idle"]); err != nil {
		return err
	}
	defer b.rm("work")
	out, err := b.exec("work", "python3", "-c", workload)
	if err != nil {
		return err
	}
	var r map[string]float64
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil {
		return fmt.Errorf("workload output: %v: %s", err, out)
	}
	names := []struct{ k, n string }{
		{"fs.create", "文件系统：创建 5000 个小文件（overlay rootfs）"},
		{"fs.stat", "文件系统：stat 5000 次"},
		{"fs.chmod", "文件系统：chmod 5000 次（元数据写）"},
		{"fs.unlink", "文件系统：删除 5000 个文件"},
		{"fs.write256m", "顺序写 256 MiB + fsync"},
		{"store.readtree", "读取整个 python store path（只读 /nix/store）"},
		{"cpu.compileall", "编译 Python 标准库（CPU + 文件 IO）"},
		{"git.commit_clone", "git：1000 个文件 init/commit/clone"},
		{"node.start5", "node 冷启动 ×5"},
		{"python.start5", "python 启动并 import json/ssl/sqlite3 ×5"},
	}
	for _, x := range names {
		if v, ok := r[x.k]; ok {
			b.add(g, x.k, x.n, "ms", true, v)
		}
	}
	if r["net.connect"] >= 0 {
		b.add(g, "net.connect", "容器 → 外网 TCP 建连（1.1.1.1:443，中位数）", "ms", true, r["net.connect"]).Note =
			"经公网，主要反映 runner 的网络；oar 自身的网络路径见 net.local"
	} else {
		b.fail(g, "net.connect", "容器 → 外网 TCP 建连", "无法连接（guest 无网络）")
	}
	if r["net.dns"] >= 0 {
		b.add(g, "net.dns", "容器内 DNS 解析", "ms", true, r["net.dns"])
	} else {
		b.fail(g, "net.dns", "容器内 DNS 解析", "解析失败")
	}
	b.netLocal(g)

	// page cache sharing: read the same store path from two containers
	// icu4c is in the closure but not touched by the workload above
	readCmd := "find $(ls -d /nix/store/*-icu4c-78* | grep -v dev | head -1) -type f -exec cat {} + > /dev/null"
	if b.backend != "native" {
		s0, err := b.statsOpt(true)
		if err != nil {
			return err
		}
		b.exec("work", "sh", "-c", readCmd)
		s1, _ := b.stats()
		first := float64(s1.Cached - s0.Cached)
		if _, _, err := b.run("work2", b.digests["idle"]); err == nil {
			// the container's own startup reads files too; measure only
			// the read itself
			time.Sleep(2 * time.Second)
			s1, _ = b.stats()
			b.exec("work2", "sh", "-c", readCmd)
			s2, _ := b.stats()
			b.rm("work2")
			d1, d2 := first, float64(s2.Cached-s1.Cached)
			if d1 > 1<<20 {
				b.add(g, "pagecache.shared", "page cache 共享：第二个容器读同一 store path 新增缓存 / 第一个", "%", true, 100*math.Max(0, d2)/d1).Note =
					fmt.Sprintf("第一个容器 +%.0f MiB，第二个 +%.0f MiB（越低越好）", d1/(1<<20), d2/(1<<20))
			} else {
				b.na(g, "pagecache.shared", "page cache 共享", "首次读取未产生可测的缓存增长（可能已在缓存中）")
			}
		}
	} else {
		b.na(g, "pagecache.shared", "page cache 共享", "native 后端：宿主机页缓存受其他进程干扰，不测")
	}
	return nil
}

// ---- phase 7: density ----

func (b *bench) phaseDensity() error {
	const g = "7-density"
	k := 10
	if !b.quick {
		k = 30
	}
	before, err := b.stats()
	if err != nil {
		return err
	}
	var lat []float64
	for i := 0; i < k; i++ {
		_, d, err := b.run(fmt.Sprintf("d%d", i), b.digests["idle"])
		if err != nil {
			return err
		}
		lat = append(lat, ms(d))
	}
	time.Sleep(time.Second)
	after, err := b.stats()
	if err != nil {
		return err
	}
	if b.backend != "native" {
		b.add(g, "mem.per", "每个空闲容器的内存开销（含 my-agent 进程）", "MiB", true, float64(before.MemAvailable-after.MemAvailable)/float64(k)/(1<<20))
	} else {
		b.na(g, "mem.per", "每个空闲容器的内存开销", "native：宿主机内存受其他进程干扰，不测")
	}
	b.add(g, "disk.per", "每个容器的磁盘开销（实例目录）", "KiB", true, float64(after.CacheBytes["instances"]-before.CacheBytes["instances"])/float64(k)/1024)
	b.add(g, "mounts.per", "每个运行中容器增加的 guest 挂载数", "个", true, float64(after.Mounts-before.Mounts)/float64(k))
	head, tail := lat[:3], lat[len(lat)-3:]
	b.add(g, "start.loaded", fmt.Sprintf("已有 %d 个容器运行时的启动延迟（最后 3 个）", k-1), "ms", true, tail...).Note =
		fmt.Sprintf("前 3 个：%.0f/%.0f/%.0f ms", head[0], head[1], head[2])
	for i := 0; i < k; i++ {
		b.rm(fmt.Sprintf("d%d", i))
	}

	// concurrent starts
	var mu sync.Mutex
	var wg sync.WaitGroup
	var conc, cCompose, cInst, cCreate, cStart, cGuest []float64
	var errs []string
	t := time.Now()
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, d, err := b.run(fmt.Sprintf("c%d", i), b.digests["idle"])
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err.Error())
				return
			}
			conc = append(conc, ms(d))
			t := r.Result.Timings
			cCompose = append(cCompose, t.Compose/1e6)
			cInst = append(cInst, t.Instantiate/1e6)
			cCreate = append(cCreate, t.Create/1e6)
			cStart = append(cStart, t.Start/1e6)
			cGuest = append(cGuest, t.Total/1e6)
		}(i)
	}
	wg.Wait()
	allWall := time.Since(t)
	if len(conc) > 0 {
		b.add(g, "concurrent", fmt.Sprintf("同时启动 %d 个容器：单个延迟", k), "ms", true, conc...).Note =
			fmt.Sprintf("全部完成 %.0f ms，吞吐 %.1f 个/s，失败 %d；guest 内中位数：总计 %.0f ms = compose %.1f + 实例化 %.1f + runc create %.0f + runc start %.0f ms",
				ms(allWall), float64(len(conc))/allWall.Seconds(), len(errs),
				median(cGuest), median(cCompose), median(cInst), median(cCreate), median(cStart))
	}
	for i := 0; i < k; i++ {
		b.rm(fmt.Sprintf("c%d", i))
	}
	// memory the VM keeps on the host after its containers are gone (memory
	// the guest freed is only returned with free page reporting)
	if rss, _, _ := b.idleResources(5 * time.Second); rss > 0 {
		b.add(g, "vm.rss.after", fmt.Sprintf("同时运行 %d 个容器并删除后：宿主机上 VM 进程树内存", k), "MiB", true, rss/(1<<20))
	}

	// many bundles cached in the guest
	vs := 10
	for i := 0; i < vs; i++ {
		name := fmt.Sprintf("v%d", i)
		if _, _, err := b.build(name, b.writeConfig(name, cfgOpts{env: map[string]string{"VARIANT": name}})); err != nil {
			return err
		}
		if _, _, err := b.run("vb", b.digests[name]); err != nil {
			return err
		}
		b.rm("vb")
	}
	var many []float64
	for i := 0; i < 5; i++ {
		_, d, err := b.run("vb", b.digests["idle"])
		if err != nil {
			return err
		}
		many = append(many, ms(d))
		b.rm("vb")
	}
	st, _ := b.stats()
	b.add(g, "bundles", fmt.Sprintf("guest 已缓存 %d+ 个组合后的启动延迟", vs), "ms", true, many...).Note =
		fmt.Sprintf("guest 挂载 %d 个，已挂载视图 %d 个", st.Mounts, st.StoreViews)
	return nil
}

// ---- phase 5: switch ----

func (b *bench) probeSummary() (map[string]float64, error) { return b.probeSummaryFor("probe") }

func (b *bench) probeSummaryFor(name string) (map[string]float64, error) {
	if _, _, err := b.oarCmd("stop", "--name", name); err != nil {
		return nil, err
	}
	out, _, err := b.oarCmd("logs", "--name", name)
	b.oarCmd("rm", "--name", name)
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if j, ok := strings.CutPrefix(l, "probe-summary "); ok {
			var m map[string]float64
			return m, json.Unmarshal([]byte(j), &m)
		}
	}
	return nil, fmt.Errorf("probe printed no summary")
}

func (b *bench) switchSeries(strategy, a, bb string, n int, prepare bool) (wall, guest, down, stop []float64, err error) {
	if _, _, err = b.run("svc", b.digests[a]); err != nil {
		return
	}
	if _, _, err = b.run("probe", b.digests["probe"]); err != nil {
		return
	}
	time.Sleep(time.Second)
	for i := 0; i < n; i++ {
		target := bb
		if i%2 == 1 {
			target = a
		}
		if prepare {
			if _, e := b.oarJSON(nil, "--bundle", b.digests[target], "prepare", "--name", "svc"); e != nil {
				err = e
				return
			}
		}
		var r runOut
		d, e := b.oarJSON(&r, "--bundle", b.digests[target], "reload", "--name", "svc", "--strategy", strategy)
		if e != nil {
			err = e
			return
		}
		wall = append(wall, ms(d))
		guest = append(guest, r.Result.Timings.Total/1e6)
		down = append(down, r.Result.Timings.Downtime/1e6)
		stop = append(stop, r.Result.Timings.StopOld/1e6)
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	return
}

func (b *bench) phaseSwitch() error {
	const g = "5-switch"
	n := 10
	if !b.quick {
		n = 30
	}
	for _, sc := range []struct {
		strategy, a, b, label, key string
		prepare                    bool
	}{
		{"overlap", "a", "b", "overlap + SO_REUSEPORT", "overlap", false},
		{"replace", "a-plain", "b-plain", "replace", "replace", false},
		{"overlap", "a", "b", "overlap + 预创建（prepare）", "prepared", true},
	} {
		wall, guest, down, stop, err := b.switchSeries(sc.strategy, sc.a, sc.b, n, sc.prepare)
		if err != nil {
			b.rm("svc")
			b.rm("probe")
			return err
		}
		sum, err := b.probeSummary()
		b.rm("svc")
		if err != nil {
			return err
		}
		b.add(g, sc.key+".wall", sc.label+"：切换耗时（CLI 墙钟）", "ms", true, wall...)
		b.add(g, sc.key+".guest", sc.label+"：切换耗时（guest 内）", "ms", true, guest...)
		b.add(g, sc.key+".stopold", sc.label+"：等待旧实例退出（StopOld，guest 内）", "ms", true, stop...)
		if sc.strategy == "replace" {
			b.add(g, sc.key+".down", sc.label+"：服务端停机窗口", "ms", true, down...)
		}
		total := sum["ok"] + sum["failed"]
		b.add(g, sc.key+".failed", fmt.Sprintf("%s：客户端失败请求（%d 次切换）", sc.label, n), "个", true, sum["failed"]).Note =
			fmt.Sprintf("共 %.0f 个请求，失败率 %.2f%%", total, 100*sum["failed"]/math.Max(total, 1))
		b.add(g, sc.key+".gap", sc.label+"：客户端两次成功响应最长间隔", "ms", true, sum["maxGapMs"])
	}

	if !b.noIngress {
		if err := b.ingressScenarios(n); err != nil {
			return err
		}
	}

	// rollback
	if _, _, err := b.run("svc", b.digests["a-plain"]); err != nil {
		return err
	}
	t := time.Now()
	_, _, rerr := b.oarCmd("--bundle", b.digests["broken"], "reload", "--name", "svc")
	rb := time.Since(t)
	var list proto.ListResult
	b.oarJSON(&list, "ps")
	alive := false
	for _, c := range list.Containers {
		alive = alive || (c.Name == "svc" && c.Status == "running" && c.BundleDigest == b.digests["a-plain"])
	}
	m := b.add(g, "rollback", "回滚：新版本未就绪 → 拒绝并保留旧实例", "ms", true, ms(rb))
	m.Note = fmt.Sprintf("reload 返回错误=%v，旧实例仍在服务=%v", rerr != nil, alive)
	if rerr == nil || !alive {
		m.Status = "fail"
	}
	b.rm("svc")

	// slow-starting application
	if _, _, err := b.run("svc", b.digests["slow-a"]); err != nil {
		return err
	}
	var slow []float64
	for i := 0; i < 3; i++ {
		target := "slow-b"
		if i%2 == 1 {
			target = "slow-a"
		}
		d, err := b.oarJSON(nil, "--bundle", b.digests[target], "reload", "--name", "svc")
		if err != nil {
			return err
		}
		slow = append(slow, ms(d))
	}
	b.add(g, "slow", "慢启动应用（初始化 3 秒）的切换耗时", "ms", true, slow...).Note = "受应用初始化时间限制；快照（P1）的目标场景"
	b.rm("svc")

	// slow-stopping application: the old instance needs 1 s to exit after
	// SIGTERM, so the reload includes waiting for it
	if _, _, err := b.run("svc", b.digests["stop-a"]); err != nil {
		return err
	}
	var stWall, stGuest, stStop []float64
	for i := 0; i < 5; i++ {
		target := "stop-b"
		if i%2 == 1 {
			target = "stop-a"
		}
		var r runOut
		d, err := b.oarJSON(&r, "--bundle", b.digests[target], "reload", "--name", "svc")
		if err != nil {
			return err
		}
		stWall = append(stWall, ms(d))
		stGuest = append(stGuest, r.Result.Timings.Total/1e6)
		stStop = append(stStop, r.Result.Timings.StopOld/1e6)
		time.Sleep(300 * time.Millisecond)
	}
	b.add(g, "slowstop.wall", "慢退出应用（SIGTERM 后 1 秒才退出）：切换耗时（CLI 墙钟）", "ms", true, stWall...)
	b.add(g, "slowstop.guest", "慢退出应用：切换耗时（guest 内）", "ms", true, stGuest...)
	b.add(g, "slowstop.stopold", "慢退出应用：等待旧实例退出（StopOld，guest 内）", "ms", true, stStop...).Note =
		"切换结果等旧实例退出后才返回；实验 B 可把这部分移出关键路径"
	b.rm("svc")

	// stability: many consecutive switches, then check for leaks
	m2 := 30
	if !b.quick {
		m2 = 100
	}
	if _, _, err := b.run("svc", b.digests["a"]); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	s0, _ := b.stats()
	var lat []float64
	for i := 0; i < m2; i++ {
		target := "b"
		if i%2 == 1 {
			target = "a"
		}
		d, err := b.oarJSON(nil, "--bundle", b.digests[target], "reload", "--name", "svc")
		if err != nil {
			return err
		}
		lat = append(lat, ms(d))
	}
	time.Sleep(2 * time.Second) // background cleanup
	s1, _ := b.stats()
	b.add(g, "stability", fmt.Sprintf("连续切换 %d 次：切换耗时分布", m2), "ms", true, lat...).Note =
		fmt.Sprintf("前 10 次 p50 %.0f ms，后 10 次 p50 %.0f ms", median(lat[:10]), median(lat[len(lat)-10:]))
	leak := b.add(g, "leak", fmt.Sprintf("连续切换 %d 次后的资源泄漏（挂载/cgroup/实例数变化）", m2), "个", true,
		float64((s1.Mounts-s0.Mounts)+(s1.Cgroups-s0.Cgroups)+(s1.Instances-s0.Instances)))
	leak.Note = fmt.Sprintf("挂载 %d→%d，cgroup %d→%d，实例 %d→%d，实例目录 %.1f→%.1f MiB",
		s0.Mounts, s1.Mounts, s0.Cgroups, s1.Cgroups, s0.Instances, s1.Instances,
		float64(s0.CacheBytes["instances"])/(1<<20), float64(s1.CacheBytes["instances"])/(1<<20))
	b.rm("svc")
	return nil
}

// ingressScenarios measures switching through oar's ingress proxy (no
// SO_REUSEPORT), hot standby for slow-starting apps, and host access.
func (b *bench) ingressScenarios(n int) error {
	const g = "5-switch"
	// 1. overlap switches through the ingress
	if _, _, err := b.run("svc", b.digests["ing-a"]); err != nil {
		return err
	}
	if _, _, err := b.run("probe-ing", b.digests["probe-ing"]); err != nil {
		return err
	}
	time.Sleep(time.Second)
	var wall, guest, drainT, stopT []float64
	for i := 0; i < n; i++ {
		target := "ing-b"
		if i%2 == 1 {
			target = "ing-a"
		}
		var r runOut
		d, err := b.oarJSON(&r, "--bundle", b.digests[target], "reload", "--name", "svc")
		if err != nil {
			return err
		}
		wall = append(wall, ms(d))
		guest = append(guest, r.Result.Timings.Total/1e6)
		drainT = append(drainT, r.Result.Timings.Drain/1e6)
		stopT = append(stopT, r.Result.Timings.StopOld/1e6)
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	sum, err := b.probeSummaryFor("probe-ing")
	if err != nil {
		return err
	}
	b.add(g, "ingress.wall", "入口代理切换：切换耗时（CLI 墙钟）", "ms", true, wall...)
	b.add(g, "ingress.guest", "入口代理切换：切换耗时（guest 内）", "ms", true, guest...)
	b.add(g, "ingress.drain", "入口代理切换：旧实例排空在途连接", "ms", true, drainT...)
	b.add(g, "ingress.stopold", "入口代理切换：等待旧实例退出（StopOld，guest 内）", "ms", true, stopT...)
	total := sum["ok"] + sum["failed"]
	b.add(g, "ingress.failed", fmt.Sprintf("入口代理切换：客户端失败请求（%d 次切换，无需 SO_REUSEPORT）", n), "个", true, sum["failed"]).Note =
		fmt.Sprintf("共 %.0f 个请求，失败率 %.2f%%", total, 100*sum["failed"]/math.Max(total, 1))
	b.add(g, "ingress.gap", "入口代理切换：客户端两次成功响应最长间隔", "ms", true, sum["maxGapMs"])

	// 3. host -> container through the ingress (and the VM forwarder)
	b.hostAccess()
	b.rm("svc")

	// 2. slow-starting app with a hot standby
	if _, _, err := b.run("svc", b.digests["slowi-a"]); err != nil {
		return err
	}
	var prep, sw, swStop []float64
	for i := 0; i < 3; i++ {
		target := "slowi-b"
		if i%2 == 1 {
			target = "slowi-a"
		}
		d, err := b.oarJSON(nil, "--bundle", b.digests[target], "prepare", "--name", "svc", "--start")
		if err != nil {
			return err
		}
		prep = append(prep, ms(d))
		var r runOut
		d, err = b.oarJSON(&r, "--bundle", b.digests[target], "reload", "--name", "svc")
		if err != nil {
			return err
		}
		sw = append(sw, ms(d))
		swStop = append(swStop, r.Result.Timings.StopOld/1e6)
	}
	b.add(g, "standby.switch", "慢启动应用（初始化 3 秒）+ 热备：切换耗时", "ms", true, sw...).Note =
		fmt.Sprintf("热备准备（prepare --start，提前进行，不计入切换）p50 %.0f ms", median(prep))
	b.add(g, "standby.stopold", "热备切换：等待旧实例退出（StopOld，guest 内）", "ms", true, swStop...)
	b.rm("svc")
	return nil
}

// hostAccess measures requests from the host to the ingress port.
func (b *bench) hostAccess() {
	const g = "6-runtime"
	url := fmt.Sprintf("http://127.0.0.1:%d/", ingressPort)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var ok bool
	for i := 0; i < 50; i++ { // the VM forwarder polls the guest for ports
		if r, err := client.Get(url); err == nil {
			r.Body.Close()
			ok = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ok {
		b.fail(g, "host.latency", "宿主机 → 容器服务（入口代理）", "宿主机无法访问入口端口")
		return
	}
	var lat []float64
	for i := 0; i < 100; i++ {
		t := time.Now()
		r, err := client.Get(url)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		lat = append(lat, ms(time.Since(t)))
	}
	b.add(g, "host.latency", "宿主机 → 容器服务：HTTP 请求延迟（每次新建连接）", "ms", true, lat...)
	var tput []float64
	for i := 0; i < 3; i++ {
		t := time.Now()
		r, err := client.Get(url + "blob?mib=64")
		if err != nil {
			continue
		}
		n, _ := io.Copy(io.Discard, r.Body)
		r.Body.Close()
		tput = append(tput, float64(n)/(1<<20)/time.Since(t).Seconds())
	}
	b.add(g, "host.tput", "宿主机 → 容器服务：下载吞吐（64 MiB）", "MiB/s", false, tput...)
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	return s[len(s)/2]
}

// ---- phase 9: application snapshots ----

func (b *bench) phaseSnapshot() error {
	const g = "9-snapshot"
	// first start: application start-up (3 s) plus the checkpoint
	r, wall, err := b.run("snap", b.digests["snap-a"])
	if err != nil {
		return err
	}
	t := r.Result.Timings
	if r.Result.SnapshotNote != "" || t.Snapshot == 0 {
		note := r.Result.SnapshotNote
		if note == "" {
			note = "没有生成快照"
		}
		b.rm("snap")
		b.na(g, "take", "打快照", note)
		return nil
	}
	b.add(g, "first", "首次启动（应用初始化 3 秒 + 打快照）", "ms", true, ms(wall)).Note =
		fmt.Sprintf("就绪 %.0f ms，打快照 %.0f ms", t.Ready/1e6, t.Snapshot/1e6)
	b.add(g, "take", "打快照耗时（应用短暂冻结）", "ms", true, t.Snapshot/1e6)
	b.add(g, "size", "快照大小（CRIU 镜像 + 可写层）", "MiB", true, float64(r.Result.SnapshotBytes)/(1<<20))
	b.rm("snap")

	// cold start / crash restart: restore instead of start
	var e2e, restore []float64
	served := true
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 5; i++ {
		r, wall, err := b.run("snap", b.digests["snap-a"])
		if err != nil {
			return err
		}
		if !r.Result.Restored {
			b.rm("snap")
			b.fail(g, "restore", "从快照恢复启动", "没有走恢复路径："+r.Result.SnapshotNote)
			return nil
		}
		e2e = append(e2e, ms(wall))
		restore = append(restore, r.Result.Timings.Restore/1e6)
		if b.backend == "native" { // VM backends: the forwarder checks below
			if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", ingressPort)); err != nil || resp.Header.Get("X-Variant") != "snap-a" {
				served = false
			} else {
				resp.Body.Close()
			}
		}
		b.rm("snap")
	}
	b.add(g, "cold", "冷启动 / 崩溃重启：从快照恢复（端到端）", "ms", true, e2e...).Note = "对比：不用快照需等应用初始化 3 秒以上"
	b.add(g, "restore", "　其中：runc restore", "ms", true, restore...)
	m := b.add(g, "served", "恢复出的实例直接提供服务", "通过", false, boolVal(served))
	if !served {
		m.Status = "fail"
	}

	// switching between two snapshotted bundles without a standby
	if _, _, err := b.run("snap", b.digests["snap-b"]); err != nil { // snapshot b
		return err
	}
	if _, err := b.oarJSON(nil, "--bundle", b.digests["snap-a"], "reload", "--name", "snap"); err != nil {
		return err
	}
	var sw []float64
	restoredAll := true
	for i := 0; i < 4; i++ {
		target := "snap-b"
		if i%2 == 1 {
			target = "snap-a"
		}
		var r runOut
		d, err := b.oarJSON(&r, "--bundle", b.digests[target], "reload", "--name", "snap")
		if err != nil {
			return err
		}
		restoredAll = restoredAll && r.Result.Restored
		sw = append(sw, ms(d))
	}
	b.rm("snap")
	b.add(g, "switch", "慢启动应用切换：从快照恢复（无需热备）", "ms", true, sw...).Note =
		fmt.Sprintf("全部走恢复路径=%v", restoredAll)
	return nil
}

// ---- phase 8: correctness ----

func (b *bench) phaseCorrectness() error {
	const g = "8-correctness"
	// determinism: shuffled component order
	base := []string{"nix:nodejs_22", "nix:python313", "nix:git", "binary:my-agent"}
	same := true
	for i := 0; i < 3; i++ {
		perm := append([]string(nil), base...)
		rand.Shuffle(len(perm), func(x, y int) { perm[x], perm[y] = perm[y], perm[x] })
		name := fmt.Sprintf("shuf%d", i)
		if _, _, err := b.build(name, b.writeConfig(name, cfgOpts{components: perm, env: map[string]string{"VARIANT": "idle"}})); err != nil {
			return err
		}
		same = same && b.digests[name] == b.digests["idle"]
	}
	m := b.add(g, "determinism", "确定性：3 种打乱顺序得到相同 digest", "通过", false, boolVal(same))
	if !same {
		m.Status = "fail"
	}
	// closure-only store view
	if _, _, err := b.run("cc", b.digests["idle"]); err != nil {
		return err
	}
	out, err := b.exec("cc", "sh", "-c", "ls /nix/store | wc -l; touch /nix/store/x 2>/dev/null && echo writable || echo ro")
	b.rm("cc")
	if err == nil {
		var pl manifest.PlatformLock
		manifest.ReadJSON(filepath.Join(b.art, "manifests", "bundles", strings.TrimPrefix(b.digests["idle"], "sha256:")+".json"), &pl)
		f := strings.Fields(out)
		ok := len(f) == 2 && f[0] == strconv.Itoa(len(pl.StorePaths)) && f[1] == "ro"
		m := b.add(g, "closure", "容器只看到自己的闭包且只读", "通过", false, boolVal(ok))
		m.Note = fmt.Sprintf("/nix/store 条目 %s，闭包 %d，%s", f[0], len(pl.StorePaths), strings.Join(f[1:], ""))
		if !ok {
			m.Status = "fail"
		}
	}
	// tampered lock is rejected
	src := filepath.Join(b.art, "manifests", "bundles", strings.TrimPrefix(b.digests["idle"], "sha256:")+".json")
	var pl manifest.PlatformLock
	if err := manifest.ReadJSON(src, &pl); err == nil {
		pl.Process.Args = []string{"/bin/sh", "-c", "evil"}
		fake := filepath.Join(b.art, "manifests", "bundles", "ffff"+strings.TrimPrefix(b.digests["idle"], "sha256:")[4:]+".json")
		manifest.WriteJSON(fake, &pl)
		_, _, err := b.oarCmd("--bundle", "ffff"+strings.TrimPrefix(b.digests["idle"], "sha256:")[4:16], "compose")
		os.Remove(fake)
		m := b.add(g, "tamper.lock", "篡改的 bundle lock 被拒绝", "通过", false, boolVal(err != nil))
		if err == nil {
			m.Status = "fail"
		}
	}
	// corrupted artifact upload is rejected
	body := strings.NewReader(strings.Repeat("x", 4096))
	err = b.client().CallBody(context.Background(), proto.OpWarmPut, proto.WarmPutArgs{Item: proto.WarmItem{
		Kind: proto.KindComponent, Key: "sha256:" + strings.Repeat("ab", 32), Size: 4096,
	}}, body, 4096, nil, nil)
	m = b.add(g, "tamper.artifact", "校验和不匹配的产物上传被拒绝", "通过", false, boolVal(err != nil))
	if err == nil {
		m.Status = "fail"
	}
	return nil
}

// ---- phase 3b: restart ----

func (b *bench) phaseVMRestart() error {
	const g = "3-vm"
	if _, _, err := b.oarCmd("vm", "stop"); err != nil {
		return err
	}
	if b.root != "" {
		out, _ := os.ReadFile("/proc/self/mountinfo")
		leaks := strings.Count(string(out), b.root+"/")
		m := b.add("8-correctness", "leak.stop", "停止后宿主机无残留挂载（native）", "个", true, float64(leaks))
		if leaks != 0 {
			m.Status = "fail"
		}
	}
	args := []string{"vm", "start", "--backend", b.backend, "--arch", b.arch, "--guest-dir", filepath.Join(b.guestDir(), b.arch)}
	if b.root != "" {
		args = append(args, "--root", b.root)
	}
	args = append(args, b.vmArgs...)
	_, wall, err := b.oarCmd(args...)
	if err != nil {
		return err
	}
	b.add(g, "boot.warm", "虚拟机重启（数据盘已存在）", "ms", true, ms(wall))
	var w warmOut
	if _, err := b.oarJSON(&w, "--bundle", b.digests["a"], "warm"); err == nil {
		m := b.add(g, "persist", "重启后缓存仍在（无需重新预热）", "需上传产物数", true, float64(w.Uploaded))
		if w.Uploaded != 0 {
			m.Status = "fail"
		}
	}
	return nil
}

// saveVMLogs copies the VM's console and supervisor logs next to the results
// after a failed phase (<out>.<phase>.console.log, ...), for diagnosis.
func (b *bench) saveVMLogs(out, phase string) {
	dir := vm.Dir(b.home, "default")
	base := strings.TrimSuffix(out, ".json")
	for _, name := range []string{"console.log", "supervisor.log", "qemu.log"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			os.WriteFile(fmt.Sprintf("%s.%s.%s", base, phase, name), data, 0o644)
		}
	}
}

// netLocalScript connects from the container to a TCP listener on the host
// (argv: host or "gateway", port) and prints the connect latencies.
const netLocalScript = `
import json, socket, struct, sys, time
host, port = sys.argv[1], int(sys.argv[2])
if host == "gateway":
    for line in open("/proc/net/route").readlines()[1:]:
        f = line.split()
        if f[1] == "00000000":
            host = socket.inet_ntoa(struct.pack("<L", int(f[2], 16)))
            break
lat = []
for _ in range(10):
    s = time.time()
    socket.create_connection((host, port), timeout=3).close()
    lat.append((time.time() - s) * 1000)
print(json.dumps({"host": host, "lat": lat}))
`

// netLocal measures TCP connect latency from a container to the host:
// the container's own network path (virtual NIC, NAT), independent of the
// runner's internet connection. Native containers share the host's network
// namespace and use loopback; guests reach the host as their gateway.
func (b *bench) netLocal(g string) {
	const name = "容器 → 宿主机 TCP 建连（虚拟网卡 + NAT，10 次）"
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		b.fail(g, "net.local", name, err.Error())
		return
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	host := "gateway"
	if b.backend == "native" {
		host = "127.0.0.1"
	}
	out, err := b.exec("work", "python3", "-c", netLocalScript, host, strconv.Itoa(l.Addr().(*net.TCPAddr).Port))
	if err != nil {
		b.fail(g, "net.local", name, err.Error())
		return
	}
	var r struct {
		Host string    `json:"host"`
		Lat  []float64 `json:"lat"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil || len(r.Lat) == 0 {
		b.fail(g, "net.local", name, fmt.Sprintf("output: %v: %s", err, out))
		return
	}
	b.add(g, "net.local", name, "ms", true, r.Lat...).Note = "目标 " + r.Host
}
