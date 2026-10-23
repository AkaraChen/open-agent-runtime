package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
	"github.com/AkaraChen/open-agent-runtime/internal/vm"
)

type vmStartOpts struct {
	backend    string
	arch       string
	cpus       int
	memory     int
	disk       int
	guestDir   string
	cmdline    string
	root       string
	foreground bool
}

func vmCmd(g *globalOpts) *cobra.Command {
	c := &cobra.Command{Use: "vm", Short: "Manage the resident Linux guest"}
	o := &vmStartOpts{}
	start := &cobra.Command{
		Use:   "start",
		Short: "Boot the resident guest (no-op if it is already running)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return startVM(cmd.Context(), g, o, true)
		},
	}
	addStartFlags(start, o)
	start.Flags().BoolVar(&o.foreground, "foreground", false, "run the supervisor in the foreground")

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Shut the guest down (all containers stop)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return stopVM(cmd.Context(), g)
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show guest status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := vm.Dir(vm.Home(), g.vmName)
			st, err := vm.ReadState(dir)
			if err != nil || !processAlive(st.Pid) {
				g.print(map[string]any{"name": g.vmName, "running": false}, func() { fmt.Printf("vm %q is not running\n", g.vmName) })
				return nil
			}
			var info proto.InfoResult
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			err = vm.Client(dir).Call(ctx, proto.OpInfo, nil, &info)
			out := map[string]any{"name": g.vmName, "running": err == nil, "state": st, "agent": info}
			g.print(out, func() {
				fmt.Printf("vm %q: backend=%s pid=%d up since %s (boot %s)\n", g.vmName, st.Backend, st.Pid,
					st.Started.Format(time.RFC3339), st.Boot.Round(time.Millisecond))
				if err != nil {
					fmt.Printf("  agent unreachable: %v\n", err)
					return
				}
				runc := info.Runc
				if runc == "" {
					runc = "runc version pending" // the agent looks it up in the background after it starts
				}
				fmt.Printf("  guest: linux/%s kernel %s, mode %s, %s\n  cache: %s, containers: %d\n",
					info.Arch, info.Kernel, info.Mode, runc, info.Root, info.Containers)
			})
			return nil
		},
	}
	var supDir, supOwner string
	var daemonize bool
	supervise := &cobra.Command{
		Use:    "supervise",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return supervise(cmd.Context(), supDir, supOwner, daemonize)
		},
	}
	supervise.Flags().StringVar(&supDir, "dir", "", "")
	supervise.Flags().StringVar(&supOwner, "owner", "", "")
	supervise.Flags().BoolVar(&daemonize, "daemonize", false, "")
	backends := &cobra.Command{
		Use:   "backends",
		Short: "List VM backends and whether they are usable here",
		Run: func(cmd *cobra.Command, _ []string) {
			for _, n := range vm.Names() {
				b, _ := vm.Get(n)
				status := "available"
				if err := b.Available(); err != nil {
					status = err.Error()
				}
				def := ""
				if n == vm.DefaultBackend() {
					def = " (default)"
				}
				fmt.Printf("%-8s%s %s\n", n, def, status)
			}
		},
	}
	statsCmd := &cobra.Command{
		Use:   "stats",
		Short: "Show guest resource usage (cache sizes, mounts, memory, boot timeline)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := connect(cmd.Context(), g)
			if err != nil {
				return err
			}
			var st proto.StatsResult
			if err := cl.Call(cmd.Context(), proto.OpStats, nil, &st); err != nil {
				return err
			}
			g.print(st, func() {
				fmt.Printf("memory: %s available of %s (page cache %s)\n", human(st.MemAvailable), human(st.MemTotal), human(st.Cached))
				fmt.Printf("mounts %d, store views %d (%d taken apart for another closure, %d built again since), instances %d, cgroups %d, load %.2f\n",
					st.Mounts, st.StoreViews, st.ViewSteals, st.ViewRebuilds, st.Instances, st.Cgroups, st.Load1)
				for _, k := range sortedKeys(st.CacheBytes) {
					fmt.Printf("cache %-10s %10s  %8d files\n", k, human(st.CacheBytes[k]), st.CacheFiles[k])
				}
			})
			return nil
		},
	}
	var profKind, profOut string
	var profSeconds int
	profile := &cobra.Command{
		Use:    "profile",
		Short:  "Fetch a Go runtime profile of the guest agent",
		Hidden: true,
		Long: "Fetches a runtime/pprof profile of the guest agent process (the supervisor on the native backend)\n" +
			"and writes it to a file. Read it with `go tool pprof FILE`; --kind trace is an execution trace\n" +
			"for `go tool trace FILE`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fetchProfile(cmd.Context(), g, profKind, profSeconds, profOut)
		},
	}
	profile.Flags().StringVar(&profKind, "kind", proto.ProfileCPU, "cpu, heap, allocs, goroutine, block, mutex or trace")
	profile.Flags().IntVar(&profSeconds, "seconds", 10, "how long cpu, trace, block and mutex sample (at most 120)")
	profile.Flags().StringVarP(&profOut, "output", "o", "", "file to write the profile to (- for stdout)")
	profile.MarkFlagRequired("output")
	c.AddCommand(start, stop, status, supervise, backends, statsCmd, profile)
	return c
}

// fetchProfile saves a runtime profile of the guest agent (see
// proto.DebugProfileArgs) to path, or to stdout for "-".
func fetchProfile(ctx context.Context, g *globalOpts, kind string, seconds int, path string) error {
	cl, err := connect(ctx, g)
	if err != nil {
		return err
	}
	out := os.Stdout
	if path != "-" {
		if out, err = os.Create(path); err != nil {
			return err
		}
	}
	w := &countWriter{w: out}
	err = cl.CallBody(ctx, proto.OpDebugProfile, proto.DebugProfileArgs{Kind: kind, Seconds: seconds}, nil, 0, nil, w)
	if path != "-" {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil && w.n == 0 {
		err = fmt.Errorf("the agent sent an empty %s profile", kind)
	}
	if err != nil {
		if path != "-" {
			os.Remove(path)
		}
		return err
	}
	if path == "-" {
		path = "stdout"
	}
	fmt.Fprintf(os.Stderr, "%s profile: %s -> %s\n", kind, human(w.n), path)
	return nil
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func addStartFlags(c *cobra.Command, o *vmStartOpts) {
	c.Flags().StringVar(&o.backend, "backend", vm.DefaultBackend(), "VM backend: "+strings.Join(vm.Names(), ", "))
	c.Flags().StringVar(&o.arch, "arch", runtime.GOARCH, "guest architecture")
	c.Flags().IntVar(&o.cpus, "cpus", 2, "guest vCPUs")
	c.Flags().IntVar(&o.memory, "memory", 2048, "guest memory (MiB)")
	c.Flags().IntVar(&o.disk, "disk", 20, "guest data disk size (GiB, sparse)")
	c.Flags().StringVar(&o.guestDir, "guest-dir", "", "directory containing kernel and initramfs.cpio.lz4")
	c.Flags().StringVar(&o.cmdline, "guest-cmdline", "", "extra guest kernel parameters (debugging)")
	c.Flags().MarkHidden("guest-cmdline")
	c.Flags().StringVar(&o.root, "root", "", "cache directory for the native backend (default /var/lib/oar)")
}

// vmRunning returns the state of a live VM, or nil.
func vmRunning(dir string) *vm.State {
	st, err := vm.ReadState(dir)
	if err != nil || !st.Ready || !processAlive(st.Pid) {
		return nil
	}
	return st
}

func startVM(ctx context.Context, g *globalOpts, o *vmStartOpts, verbose bool) error {
	home := vm.Home()
	dir := vm.Dir(home, g.vmName)
	if st := vmRunning(dir); st != nil {
		if verbose {
			fmt.Printf("vm %q already running (backend %s, pid %d)\n", g.vmName, st.Backend, st.Pid)
		}
		return nil
	}
	b, err := vm.Get(o.backend)
	if err != nil {
		return err
	}
	if err := b.Available(); err != nil {
		return fmt.Errorf("backend %s: %w", o.backend, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cfg := vm.MachineConfig{
		Name: g.vmName, Dir: dir, Arch: o.arch, CPUs: o.cpus, MemoryMiB: o.memory, DiskGiB: o.disk,
		Root: o.root, ConsoleLog: filepath.Join(dir, "console.log"), GuestCmdline: o.cmdline,
	}
	if b.NeedsGuestImage() {
		cfg.Kernel, cfg.Initrd, err = vm.GuestImage(home, o.arch, o.guestDir)
		if err != nil {
			return err
		}
		if t := filepath.Join(filepath.Dir(cfg.Kernel), "rootfs.tar"); fileExists(t) {
			cfg.RootFSTar = t
		}
	}
	mc, _ := json.MarshalIndent(map[string]any{"backend": o.backend, "machine": cfg}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "machine.json"), mc, 0o644); err != nil {
		return err
	}
	os.Remove(filepath.Join(dir, "state.json"))
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"vm", "supervise", "--dir", dir}
	if o.foreground {
		return supervise(ctx, dir, "", false)
	}
	args = append(args, "--daemonize")
	var cmd *exec.Cmd
	if b.NeedsRoot() && os.Geteuid() != 0 {
		args = append(args, "--owner", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
		cmd = exec.Command("sudo", append([]string{"--", exe}, args...)...)
	} else {
		cmd = exec.Command(exe, args...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if verbose {
		fmt.Printf("starting vm %q (backend %s)...\n", g.vmName, o.backend)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("launch supervisor: %w", err)
	}
	// PERF: poll adaptively. A fixed 100 ms sleep here rounded every "VM cold
	// boot" timed through the CLI up to a multiple of 100 ms plus the launch
	// time (the baseline's kvm 409 ms is 4 polls + 9 ms, its native 116 ms is
	// 1 poll + 16 ms), so those numbers described the poll, not the boot. A
	// pipe that the supervisor closes when it is ready would avoid polling,
	// but sudo (native backend) closes inherited file descriptors; reading
	// state.json works everywhere, and 2 ms steps are cheap for the first
	// second.
	waitStart := time.Now()
	deadline := waitStart.Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		st, err := vm.ReadState(dir)
		if err == nil {
			if st.Error != "" {
				return fmt.Errorf("vm failed to start: %s\n%s", st.Error, tail(filepath.Join(dir, "supervisor.log"), 30))
			}
			if st.Ready {
				if verbose {
					fmt.Printf("vm %q ready in %s\n", g.vmName, st.Boot.Round(time.Millisecond))
				}
				return nil
			}
			if st.Pid > 0 && !processAlive(st.Pid) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(vmPollInterval(time.Since(waitStart))):
		}
	}
	return fmt.Errorf("vm did not become ready\n%s", tail(filepath.Join(dir, "supervisor.log"), 30))
}

// vmPollInterval is how long startVM waits before it reads the supervisor's
// state again, given how long it has been waiting: short while a native or
// warm guest can be ready within milliseconds, longer once the boot drags on
// (emulated guests take seconds), so that a slow boot does not keep the CLI
// busy.
func vmPollInterval(waited time.Duration) time.Duration {
	switch {
	case waited < time.Second:
		return 2 * time.Millisecond
	case waited < 10*time.Second:
		return 20 * time.Millisecond
	}
	return 100 * time.Millisecond
}

func supervise(ctx context.Context, dir, owner string, daemonize bool) error {
	if daemonize {
		return daemonizeSelf(dir, owner)
	}
	b, err := os.ReadFile(filepath.Join(dir, "machine.json"))
	if err != nil {
		return err
	}
	var mc struct {
		Backend string           `json:"backend"`
		Machine vm.MachineConfig `json:"machine"`
	}
	if err := json.Unmarshal(b, &mc); err != nil {
		return err
	}
	be, err := vm.Get(mc.Backend)
	if err != nil {
		return err
	}
	var own *vm.Owner
	if owner != "" {
		u, gs, _ := strings.Cut(owner, ":")
		uid, err1 := strconv.Atoi(u)
		gid, err2 := strconv.Atoi(gs)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("bad --owner %q", owner)
		}
		own = &vm.Owner{UID: uid, GID: gid}
	}
	return vm.Supervise(ctx, be, mc.Machine, own, os.Stderr)
}

// daemonizeSelf re-executes the supervisor detached from the terminal,
// logging to supervisor.log, and returns immediately.
func daemonizeSelf(dir, owner string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, "supervisor.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	if owner != "" {
		var u, gid int
		fmt.Sscanf(strings.Replace(owner, ":", " ", 1), "%d %d", &u, &gid)
		os.Lchown(logPath, u, gid)
	}
	args := []string{"vm", "supervise", "--dir", dir}
	if owner != "" {
		args = append(args, "--owner", owner)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func stopVM(ctx context.Context, g *globalOpts) error {
	dir := vm.Dir(vm.Home(), g.vmName)
	st, err := vm.ReadState(dir)
	if err != nil || !processAlive(st.Pid) {
		fmt.Printf("vm %q is not running\n", g.vmName)
		os.Remove(filepath.Join(dir, "state.json"))
		return nil
	}
	b, _ := vm.Get(st.Backend)
	if err := terminate(st.Pid, b != nil && b.NeedsRoot()); err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	for processAlive(st.Pid) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if processAlive(st.Pid) {
		return fmt.Errorf("supervisor %d did not exit", st.Pid)
	}
	fmt.Printf("vm %q stopped\n", g.vmName)
	return nil
}

// ensureVM returns a client for a running guest, booting it with default
// settings if necessary.
func ensureVM(ctx context.Context, g *globalOpts) (*proto.Client, proto.InfoResult, error) {
	dir := vm.Dir(vm.Home(), g.vmName)
	var info proto.InfoResult
	if vmRunning(dir) == nil {
		o := &vmStartOpts{backend: vm.DefaultBackend(), arch: runtime.GOARCH, cpus: 2, memory: 2048, disk: 20}
		if err := startVM(ctx, g, o, !g.jsonOut); err != nil {
			return nil, info, err
		}
	}
	c := vm.Client(dir)
	if err := c.Call(ctx, proto.OpInfo, nil, &info); err != nil {
		return nil, info, err
	}
	if info.Protocol != proto.Version {
		return nil, info, fmt.Errorf("guest agent speaks protocol %d, CLI speaks %d: rebuild the guest image (guest/build.sh) and restart the VM", info.Protocol, proto.Version)
	}
	return c, info, nil
}

func tail(p string, n int) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
