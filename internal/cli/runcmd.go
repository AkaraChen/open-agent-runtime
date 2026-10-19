package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/AkaraChen/open-agent-runtime/internal/builder"
	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/proto"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
	"github.com/AkaraChen/open-agent-runtime/internal/vm"
)

func itoa(i int) string      { return strconv.Itoa(i) }
func sortStrings(s []string) { sort.Strings(s) }

// session bundles everything needed to talk to the guest about one config.
type session struct {
	g      *globalOpts
	cfg    *spec.Config
	client *proto.Client
	info   proto.InfoResult
	lock   *manifest.PlatformLock
	art    string
}

// open connects to the guest (booting it if needed) and loads the lock for
// the guest's architecture, rebuilding artifacts first unless noBuild.
func open(ctx context.Context, g *globalOpts, build bool) (*session, error) {
	cfg, err := g.loadConfig()
	if err != nil {
		return nil, err
	}
	s := &session{g: g, cfg: cfg, art: g.artifactsDir(cfg)}
	s.client, s.info, err = ensureVM(ctx, g)
	if err != nil {
		return nil, err
	}
	platform := "linux/" + s.info.Arch
	if build {
		if _, err := builder.Build(ctx, cfg, builder.Options{
			Artifacts: s.art, Platforms: []string{platform}, Log: quietLog(g),
		}); err != nil {
			return nil, err
		}
	}
	lock, err := manifest.Read(builder.LockPath(s.art))
	if err != nil {
		return nil, fmt.Errorf("%w (run oar build first)", err)
	}
	pl, ok := lock.Platforms[platform]
	if !ok {
		return nil, fmt.Errorf("lock has no %s bundle; run oar build --platform %s", platform, platform)
	}
	s.lock = pl
	return s, nil
}

func quietLog(g *globalOpts) io.Writer {
	if g.jsonOut && os.Getenv("OAR_VERBOSE") == "" {
		return io.Discard
	}
	return os.Stderr
}

func vmDir(g *globalOpts) string        { return vm.Dir(vm.Home(), g.vmName) }
func vmClient(dir string) *proto.Client { return vm.Client(dir) }

// warmItems lists the artifacts referenced by a lock.
func warmItems(art string, pl *manifest.PlatformLock) ([]proto.WarmItem, map[string]string) {
	paths := map[string]string{}
	var items []proto.WarmItem
	add := func(it proto.WarmItem, p string) {
		items = append(items, it)
		paths[it.Kind+"|"+it.Key+"|"+it.Name] = filepath.Join(art, filepath.FromSlash(p))
	}
	add(proto.WarmItem{Kind: proto.KindBase, Key: pl.Base.Key, Size: pl.Base.Size}, pl.Base.Path)
	for _, sp := range pl.StorePaths {
		add(proto.WarmItem{Kind: proto.KindStore, Key: sp.Key, Name: sp.Name(), Size: sp.Size}, "store/nix/store/"+sp.Name()+".tar")
	}
	for _, c := range pl.Components {
		add(proto.WarmItem{Kind: proto.KindComponent, Key: c.Key, Size: c.Size}, c.Path)
	}
	return items, paths
}

type warmStats struct {
	Total    int           `json:"total"`
	Uploaded int           `json:"uploaded"`
	Bytes    int64         `json:"bytes"`
	Duration time.Duration `json:"duration"`
}

// warm uploads missing artifacts into the guest cache.
func (s *session) warm(ctx context.Context, verbose bool) (*warmStats, error) {
	start := time.Now()
	items, paths := warmItems(s.art, s.lock)
	var st proto.WarmStatusResult
	if err := s.client.Call(ctx, proto.OpWarmStatus, proto.WarmStatusArgs{Items: items}, &st); err != nil {
		return nil, err
	}
	stats := &warmStats{Total: len(items), Uploaded: len(st.Missing)}
	if len(st.Missing) == 0 {
		stats.Duration = time.Since(start)
		return stats, nil
	}
	var total int64
	for _, it := range st.Missing {
		total += it.Size
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "warming guest cache: %d of %d artifacts (%s)\n", len(st.Missing), len(items), human(total))
	}
	// biggest first keeps the pipeline busy
	sort.Slice(st.Missing, func(i, j int) bool { return st.Missing[i].Size > st.Missing[j].Size })
	var sent atomic.Int64
	var mu sync.Mutex
	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(4)
	for _, it := range st.Missing {
		it := it
		eg.Go(func() error {
			p := paths[it.Kind+"|"+it.Key+"|"+it.Name]
			fi, err := os.Stat(p)
			if err != nil {
				return fmt.Errorf("artifact %s missing locally (%w); run oar build", p, err)
			}
			if err := s.putWithRetry(ectx, it, p); err != nil {
				return fmt.Errorf("warm %s %s: %w", it.Kind, firstNonEmpty(it.Name, shortKey(it.Key)), err)
			}
			n := sent.Add(fi.Size())
			if verbose {
				mu.Lock()
				fmt.Fprintf(os.Stderr, "  %3d%%  %s %s\n", n*100/max(total, 1), it.Kind, firstNonEmpty(it.Name, shortKey(it.Key)))
				mu.Unlock()
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	stats.Bytes = sent.Load()
	stats.Duration = time.Since(start)
	return stats, nil
}

// putWithRetry uploads one artifact, retrying transport failures. Errors
// reported by the agent itself (e.g. checksum mismatch) are not retried.
func (s *session) putWithRetry(ctx context.Context, it proto.WarmItem, p string) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = func() error {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			return s.client.CallBody(ctx, proto.OpWarmPut, proto.WarmPutArgs{Item: it}, f, fi.Size(), nil, nil)
		}()
		var re *proto.RemoteError
		if err == nil || errors.As(err, &re) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
	}
	return err
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func shortKey(k string) string {
	_, h, _ := strings.Cut(k, ":")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func warmCmd(g *globalOpts) *cobra.Command {
	var noBuild bool
	c := &cobra.Command{
		Use:   "warm",
		Short: "Pre-warm the guest cache (base rootfs, /nix/store paths, components)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(cmd.Context(), g, !noBuild)
			if err != nil {
				return err
			}
			st, err := s.warm(cmd.Context(), !g.jsonOut)
			if err != nil {
				return err
			}
			g.print(st, func() {
				fmt.Printf("guest cache warm: %d artifacts, %d uploaded (%s) in %s\n", st.Total, st.Uploaded, human(st.Bytes), st.Duration.Round(time.Millisecond))
			})
			return nil
		},
	}
	c.Flags().BoolVar(&noBuild, "no-build", false, "use the existing lock without rebuilding")
	return c
}

func composeCmd(g *globalOpts) *cobra.Command {
	var build bool
	c := &cobra.Command{
		Use:   "compose",
		Short: "Compose the OCI bundle in the guest (hot path) and report its digest and timing",
		RunE: func(cmd *cobra.Command, _ []string) error {
			t0 := time.Now()
			s, err := open(cmd.Context(), g, build)
			if err != nil {
				return err
			}
			if build {
				if _, err := s.warm(cmd.Context(), !g.jsonOut); err != nil {
					return err
				}
			}
			t1 := time.Now()
			var res proto.ComposeResult
			if err := s.client.Call(cmd.Context(), proto.OpCompose, proto.ComposeArgs{Lock: s.lock}, &res); err != nil {
				return err
			}
			rt := time.Since(t1)
			out := map[string]any{
				"bundleDigest": res.BundleDigest, "cached": res.Cached, "guestDuration": res.Duration,
				"roundTrip": rt, "total": time.Since(t0), "layers": res.Layers, "platform": s.lock.Platform,
			}
			g.print(out, func() {
				fmt.Printf("bundle %s (%s)\n", res.BundleDigest, s.lock.Platform)
				fmt.Printf("  compose: %s in guest, %s round trip, %s total (cached=%v)\n",
					res.Duration.Round(time.Microsecond), rt.Round(time.Microsecond), time.Since(t0).Round(time.Millisecond), res.Cached)
				fmt.Printf("  layers: %d (+ /nix/store read-only bind, %d store paths)\n", len(res.Layers), len(s.lock.StorePaths))
			})
			return nil
		},
	}
	c.Flags().BoolVar(&build, "build", false, "build and warm before composing")
	return c
}

func runLike(g *globalOpts, reload bool) *cobra.Command {
	var name string
	var noBuild bool
	use, short := "run", "Build (incrementally), warm, compose and start the container"
	if reload {
		use, short = "reload", "Rebuild and start a new instance, then retire the old one"
	}
	c := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if name == "" {
				name = g.defaultName()
			}
			t0 := time.Now()
			s, err := open(ctx, g, !noBuild)
			if err != nil {
				return err
			}
			tBuild := time.Since(t0)
			ws, err := s.warm(ctx, !g.jsonOut)
			if err != nil {
				return err
			}
			op := proto.OpRun
			if reload {
				op = proto.OpReload
			}
			var res proto.RunResult
			if err := s.client.Call(ctx, op, proto.RunArgs{Name: name, Lock: s.lock}, &res); err != nil {
				return err
			}
			out := map[string]any{"result": res, "build": tBuild, "warm": ws, "total": time.Since(t0)}
			g.print(out, func() {
				c := res.Container
				verb := "started"
				if res.Replaced != "" {
					verb = "reloaded"
				}
				fmt.Printf("%s %s (%s, pid %d)\n  bundle %s\n", verb, c.Name, c.ID, c.Pid, c.BundleDigest)
				t := res.Timings
				fmt.Printf("  compose %s (cached=%v), instantiate %s, runc create %s, start %s; guest total %s\n",
					t.Compose.Round(time.Microsecond), res.Cached, t.Instantiate.Round(time.Microsecond),
					t.Create.Round(time.Millisecond), t.Start.Round(time.Millisecond), t.Total.Round(time.Millisecond))
				if res.Replaced != "" {
					fmt.Printf("  retired %s\n", res.Replaced)
				}
			})
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "container name (default: config directory name)")
	c.Flags().BoolVar(&noBuild, "no-build", false, "use the existing lock without rebuilding")
	return c
}

func runCmd(g *globalOpts) *cobra.Command    { return runLike(g, false) }
func reloadCmd(g *globalOpts) *cobra.Command { return runLike(g, true) }

// connect returns a client to an already running guest.
func connect(ctx context.Context, g *globalOpts) (*proto.Client, error) {
	c, _, err := ensureVMNoStart(ctx, g)
	return c, err
}

func ensureVMNoStart(ctx context.Context, g *globalOpts) (*proto.Client, proto.InfoResult, error) {
	var info proto.InfoResult
	dir := vmDir(g)
	if vmRunning(dir) == nil {
		return nil, info, fmt.Errorf("vm %q is not running", g.vmName)
	}
	c := vmClient(dir)
	err := c.Call(ctx, proto.OpInfo, nil, &info)
	return c, info, err
}

func nameFlag(c *cobra.Command, name *string) {
	c.Flags().StringVar(name, "name", "", "container name (default: config directory name)")
}

func stopCmd(g *globalOpts) *cobra.Command {
	var name string
	var timeout time.Duration
	var rm bool
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop the container (SIGTERM, then SIGKILL after --timeout)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				name = g.defaultName()
			}
			cl, err := connect(cmd.Context(), g)
			if err != nil {
				return err
			}
			var ct proto.Container
			if err := cl.Call(cmd.Context(), proto.OpStop, proto.NameArgs{Name: name, Timeout: timeout}, &ct); err != nil {
				return err
			}
			if rm {
				if err := cl.Call(cmd.Context(), proto.OpDelete, proto.NameArgs{Name: name}, nil); err != nil {
					return err
				}
				ct.Status = "deleted"
			}
			g.print(ct, func() { fmt.Printf("%s %s (%s)\n", ct.Status, ct.Name, ct.ID) })
			return nil
		},
	}
	nameFlag(c, &name)
	c.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "grace period before SIGKILL")
	c.Flags().BoolVar(&rm, "rm", false, "delete the container after stopping")
	return c
}

func rmCmd(g *globalOpts) *cobra.Command {
	var name string
	var force bool
	c := &cobra.Command{
		Use:     "rm",
		Aliases: []string{"delete"},
		Short:   "Delete a stopped container and its instance directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				name = g.defaultName()
			}
			cl, err := connect(cmd.Context(), g)
			if err != nil {
				return err
			}
			if err := cl.Call(cmd.Context(), proto.OpDelete, proto.NameArgs{Name: name, Force: force}, nil); err != nil {
				return err
			}
			g.print(map[string]string{"deleted": name}, func() { fmt.Printf("deleted %s\n", name) })
			return nil
		},
	}
	nameFlag(c, &name)
	c.Flags().BoolVar(&force, "force", false, "kill and delete a running container")
	return c
}

func psCmd(g *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "ps",
		Aliases: []string{"status"},
		Short:   "List containers and their runc state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := connect(cmd.Context(), g)
			if err != nil {
				return err
			}
			var res proto.ListResult
			if err := cl.Call(cmd.Context(), proto.OpList, nil, &res); err != nil {
				return err
			}
			g.print(res, func() {
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tID\tSTATUS\tPID\tBUNDLE\tCREATED")
				for _, c := range res.Containers {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", c.Name, c.ID, c.Status, c.Pid, shortKey(c.BundleDigest), c.Created.Local().Format(time.TimeOnly))
				}
				tw.Flush()
			})
			return nil
		},
	}
}

func logsCmd(g *globalOpts) *cobra.Command {
	var name string
	var follow bool
	c := &cobra.Command{
		Use:   "logs",
		Short: "Print container stdout/stderr",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				name = g.defaultName()
			}
			cl, err := connect(cmd.Context(), g)
			if err != nil {
				return err
			}
			return cl.CallBody(cmd.Context(), proto.OpLogs, proto.LogsArgs{Name: name, Follow: follow}, nil, 0, nil, os.Stdout)
		},
	}
	nameFlag(c, &name)
	c.Flags().BoolVarP(&follow, "follow", "F", false, "follow log output")
	return c
}

func execCmd(g *globalOpts) *cobra.Command {
	var name string
	var env []string
	c := &cobra.Command{
		Use:   "exec [flags] -- COMMAND [ARGS...]",
		Short: "Run a command in the running container (non-interactive)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				name = g.defaultName()
			}
			cl, err := connect(cmd.Context(), g)
			if err != nil {
				return err
			}
			var res proto.ExecResult
			if err := cl.Call(cmd.Context(), proto.OpExec, proto.ExecArgs{Name: name, Args: args, Env: env}, &res); err != nil {
				return err
			}
			if g.jsonOut {
				g.print(res, nil)
				return nil
			}
			fmt.Fprint(os.Stdout, res.Stdout)
			fmt.Fprint(os.Stderr, res.Stderr)
			if res.ExitCode != 0 {
				return &exitError{res.ExitCode}
			}
			return nil
		},
	}
	nameFlag(c, &name)
	c.Flags().StringArrayVarP(&env, "env", "e", nil, "extra environment KEY=VALUE")
	return c
}

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("command exited with status %d", e.code) }
