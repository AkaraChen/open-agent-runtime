// Package cli implements the oar command line.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/AkaraChen/open-agent-runtime/internal/builder"
	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

// Version of the CLI.
var Version = "0.1.0"

type globalOpts struct {
	configPath string
	artifacts  string
	vmName     string
	jsonOut    bool
	home       string
}

// Main runs the CLI.
func Main(args []string) int {
	g := &globalOpts{}
	root := &cobra.Command{
		Use:           "oar",
		Short:         "Composable Linux runtime: build immutable artifacts, compose OCI bundles, run them in a resident Linux guest",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&g.configPath, "file", "f", "oar.yaml", "runtime configuration")
	root.PersistentFlags().StringVar(&g.artifacts, "artifacts", "", "artifacts directory (default: <config dir>/artifacts)")
	root.PersistentFlags().StringVar(&g.vmName, "vm", "default", "guest VM name")
	root.PersistentFlags().BoolVar(&g.jsonOut, "json", false, "machine readable output")
	root.AddCommand(
		buildCmd(g), digestCmd(g),
		vmCmd(g),
		warmCmd(g), composeCmd(g),
		runCmd(g), reloadCmd(g), stopCmd(g), rmCmd(g), psCmd(g), logsCmd(g), execCmd(g),
	)
	if c := agentCmd(); c != nil {
		root.AddCommand(c)
	}
	root.SetArgs(args)
	ctx, cancel := signalContext()
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func (g *globalOpts) loadConfig() (*spec.Config, error) {
	return spec.Load(g.configPath)
}

func (g *globalOpts) artifactsDir(cfg *spec.Config) string {
	if g.artifacts != "" {
		a, _ := filepath.Abs(g.artifacts)
		return a
	}
	if cfg != nil {
		return filepath.Join(cfg.Dir, "artifacts")
	}
	abs, _ := filepath.Abs(g.configPath)
	return filepath.Join(filepath.Dir(abs), "artifacts")
}

var nameRe = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// defaultName derives a container name from the config directory.
func (g *globalOpts) defaultName() string {
	abs, _ := filepath.Abs(g.configPath)
	n := nameRe.ReplaceAllString(filepath.Base(filepath.Dir(abs)), "-")
	n = strings.Trim(n, "-.")
	if n == "" {
		n = "main"
	}
	if len(n) > 40 {
		n = n[:40]
	}
	return n
}

func (g *globalOpts) print(v any, human func()) {
	if g.jsonOut {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
		return
	}
	human()
}

func buildCmd(g *globalOpts) *cobra.Command {
	var platforms []string
	var update bool
	c := &cobra.Command{
		Use:   "build",
		Short: "Pre-build all artifacts (base rootfs, nix closures, components) and write bundle.lock.json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.loadConfig()
			if err != nil {
				return err
			}
			start := time.Now()
			lock, err := builder.Build(cmd.Context(), cfg, builder.Options{
				Artifacts: g.artifactsDir(cfg), Platforms: platforms, Update: update, Log: logWriter(g),
			})
			if err != nil {
				return err
			}
			g.print(lock, func() {
				fmt.Printf("built in %s -> %s\n", time.Since(start).Round(time.Millisecond), builder.LockPath(g.artifactsDir(cfg)))
				printDigests(lock, platforms)
			})
			return nil
		},
	}
	c.Flags().StringSliceVar(&platforms, "platform", nil, "platforms to build (default: all declared)")
	c.Flags().BoolVar(&update, "update", false, "re-resolve the base image tag to a new digest")
	return c
}

func printDigests(lock *manifest.Lock, only []string) {
	for _, p := range sortedKeys(lock.Platforms) {
		if len(only) > 0 && !contains(only, p) {
			continue
		}
		pl := lock.Platforms[p]
		fmt.Printf("  %-12s %s  (%d store paths, %d components)\n", p, pl.BundleDigest, len(pl.StorePaths), len(pl.Components))
	}
}

func digestCmd(g *globalOpts) *cobra.Command {
	var platform string
	c := &cobra.Command{
		Use:   "digest",
		Short: "Print the bundle digest from the lock (recomputed from content)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.loadConfig()
			if err != nil {
				return err
			}
			lock, err := manifest.Read(builder.LockPath(g.artifactsDir(cfg)))
			if err != nil {
				return err
			}
			if platform == "" {
				platform = cfg.DefaultPlatform()
			}
			pl, ok := lock.Platforms[platform]
			if !ok {
				return fmt.Errorf("lock has no %s bundle; run oar build --platform %s", platform, platform)
			}
			d := pl.ComputeDigest()
			if d != pl.BundleDigest {
				return fmt.Errorf("lock is inconsistent: stored %s, computed %s", pl.BundleDigest, d)
			}
			g.print(map[string]string{"platform": platform, "bundleDigest": d}, func() { fmt.Println(d) })
			return nil
		},
	}
	c.Flags().StringVar(&platform, "platform", "", "platform (default: runtime.platform)")
	return c
}

func logWriter(g *globalOpts) *os.File {
	if g.jsonOut {
		return os.Stderr
	}
	return os.Stdout
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
