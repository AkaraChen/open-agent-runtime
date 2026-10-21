package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/AkaraChen/open-agent-runtime/internal/nixeval"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

func nixEvalCmd(g *globalOpts) *cobra.Command {
	var system, nixpkgs, mirror, backend, expr string
	var workers int
	c := &cobra.Command{
		Use:   "nix-eval [flags] ATTR...",
		Short: "Resolve nixpkgs attributes to store paths, as oar build does",
		Long: `Evaluates nixpkgs attributes with the built-in evaluator, or with
nix-instantiate on this machine or in Docker, and prints the output paths
and main program the builder records for them. This is the same expression
oar build evaluates, so what it prints is what a build gets.

Examples:
  oar nix-eval nodejs_22 python313
  oar nix-eval --system aarch64-linux --json git
  oar nix-eval --backend local nodejs_22      # compare with nix-instantiate
  oar nix-eval --expr 'pkgs.nodejs_22.version'
  oar nix-eval --nixpkgs ~/src/nixpkgs hello  # a local checkout`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if expr == "" && len(args) == 0 {
				return errors.New("give attribute names, or --expr")
			}
			if expr != "" && len(args) > 0 {
				return errors.New("--expr takes no attribute names")
			}
			cfg, _ := g.loadConfig() // optional here
			if nixpkgs == "" {
				nixpkgs = spec.DefaultNixpkgsRevision
				if cfg != nil && cfg.Runtime.Nixpkgs != "" {
					nixpkgs = cfg.Runtime.Nixpkgs
				}
			}
			if system == "" {
				p, _ := spec.ParsePlatform(spec.HostPlatform())
				system = p.NixSystem()
			}
			ev, err := nixeval.New(backend)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if mirror == "" && cfg != nil {
				mirror = cfg.Runtime.NixpkgsMirror
			}
			dir, err := nixeval.Source(ctx, nixpkgs, mirror, os.Stderr)
			if err != nil {
				return err
			}
			req := nixeval.Request{
				Nixpkgs: nixpkgs, NixpkgsDir: dir, System: system,
				Attrs: args, Expr: expr, Workers: workers,
				CacheDir: nixeval.IRCacheDir(),
			}
			out, err := ev.Eval(ctx, req)
			if err != nil {
				return err
			}
			if expr != "" || g.jsonOut {
				os.Stdout.Write(out)
				if len(out) == 0 || out[len(out)-1] != '\n' {
					fmt.Println()
				}
				return nil
			}
			res, err := nixeval.ParseAttrs(out, args)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(res))
			for n := range res {
				names = append(names, n)
			}
			sort.Strings(names)
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ATTR\tMAIN PROGRAM\tOUTPUTS")
			for _, n := range names {
				r := res[n]
				fmt.Fprintf(tw, "%s\t%s\t%s\n", n, r.MainProgram, strings.Join(r.Outputs, " "))
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&system, "system", "", "nix system to evaluate for: x86_64-linux or aarch64-linux (default: host)")
	c.Flags().StringVar(&nixpkgs, "nixpkgs", "", "nixpkgs commit hash, or a local checkout (default: runtime.nixpkgs of the config)")
	c.Flags().StringVar(&mirror, "nixpkgs-mirror", "", "base URL serving <rev>.tar.gz nixpkgs archives (default: runtime.nixpkgs_mirror of the config, else GitHub)")
	c.Flags().StringVar(&backend, "backend", "", "embedded, local or docker (default: the first available)")
	c.Flags().StringVarP(&expr, "expr", "E", "", "evaluate an expression with pkgs and lib in scope instead of attributes")
	c.Flags().IntVar(&workers, "workers", 0, "evaluator threads for the embedded backend (default: iets's own)")
	return c
}
