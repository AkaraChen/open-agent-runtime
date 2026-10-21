// Package nixeval resolves nixpkgs attribute names to store paths. It is
// what `oar nix-eval` runs, and what the builder asks through a child
// process for every nix component (docs/design/embedded-nix-evaluator.md).
//
// Three backends answer the same request: the embedded iets evaluator
// linked into the oar binary, nix-instantiate on PATH, and nix-instantiate
// inside the nixos/nix Docker image.
package nixeval

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/AkaraChen/open-agent-runtime/internal/nixeval/iets"
)

// Backend names.
const (
	Embedded = "embedded"
	Local    = "local"
	Docker   = "docker"
)

// Backends lists every backend name in preference order.
func Backends() []string { return []string{Embedded, Local, Docker} }

// Request is one evaluation: either Attrs, which yields a Result per
// attribute, or Expr, an arbitrary expression with pkgs and lib in scope.
type Request struct {
	// Nixpkgs is the revision or local checkout the caller named. The
	// nix-eval command resolves it to NixpkgsDir with Source.
	Nixpkgs string
	// NixpkgsDir is the absolute path of the checkout the backend imports.
	NixpkgsDir string
	// System is the nix system double, e.g. x86_64-linux.
	System string
	Attrs  []string
	Expr   string
	// Workers is the embedded evaluator's thread count; 0 is its default.
	Workers int
	// CacheDir holds the embedded evaluator's compiled-file cache.
	CacheDir string
	// Mirror serves nixpkgs archives (default DefaultNixpkgsMirror).
	Mirror string
}

// Result is what the builder needs of one attribute.
type Result struct {
	Outputs     []string `json:"outputs"`
	MainProgram string   `json:"mainProgram,omitempty"`
}

// Evaluator answers a Request with the strict JSON rendering of the value.
type Evaluator interface {
	Name() string
	Eval(ctx context.Context, req Request) ([]byte, error)
}

// Expression is the Nix source a Request evaluates. nixpkgs is the path of
// the checkout as the evaluator sees it (a Docker backend mounts it
// elsewhere). The attribute form returns `{ "<attr>" = { outputs;
// mainProgram; }; }`, with documentation outputs left out.
func Expression(nixpkgs string, req Request) string {
	path, _ := json.Marshal(nixpkgs)
	head := fmt.Sprintf(`let
  pkgs = import (/. + %s) { system = %q; config = { allowUnfree = false; }; overlays = []; };
  lib = pkgs.lib;
`, path, req.System)
	if req.Expr != "" {
		return head + "in (" + strings.TrimSpace(req.Expr) + ")\n"
	}
	attrs, _ := json.Marshal(req.Attrs)
	return head + fmt.Sprintf(`  docs = [ "man" "doc" "devdoc" "info" ];
  get = n: let
      d = lib.getAttrFromPath (lib.splitString "." n) pkgs;
      outs = lib.subtractLists docs (d.meta.outputsToInstall or [ "out" ]);
    in {
      outputs = map (o: d.${o}.outPath) (if outs == [] then [ "out" ] else outs);
      mainProgram = d.meta.mainProgram or "";
    };
in builtins.listToAttrs (map (n: { name = n; value = get n; }) (builtins.fromJSON %q))
`, string(attrs))
}

// ParseAttrs reads the attribute form's output and checks that every
// requested attribute has at least one output path.
func ParseAttrs(out []byte, attrs []string) (map[string]Result, error) {
	var parsed map[string]Result
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse evaluator output: %w\n%s", err, out)
	}
	for _, a := range attrs {
		r, ok := parsed[a]
		if !ok || len(r.Outputs) == 0 {
			return nil, fmt.Errorf("evaluator returned no outputs for %s", a)
		}
		for _, o := range r.Outputs {
			if !strings.HasPrefix(o, "/nix/store/") {
				return nil, fmt.Errorf("%s: output %q is not a store path", a, o)
			}
		}
	}
	return parsed, nil
}

// Available reports whether a backend can run on this machine, and why not.
func Available(name string) (bool, string) {
	switch name {
	case Embedded:
		if !iets.Linked {
			return false, "this oar binary was built without the embedded evaluator (hack/build.sh links it)"
		}
		return true, ""
	case Local:
		if _, err := exec.LookPath("nix-instantiate"); err != nil {
			return false, "nix-instantiate is not on PATH"
		}
		return true, ""
	case Docker:
		if _, err := exec.LookPath("docker"); err != nil {
			return false, "docker is not on PATH"
		}
		return true, ""
	}
	return false, fmt.Sprintf("unknown backend %q (want %s)", name, strings.Join(Backends(), ", "))
}

// Default is the backend an empty --backend means: the first available one
// in Backends order.
func Default() (string, error) {
	var reasons []string
	for _, b := range Backends() {
		ok, why := Available(b)
		if ok {
			return b, nil
		}
		reasons = append(reasons, b+": "+why)
	}
	sort.Strings(reasons)
	return "", fmt.Errorf("no nix evaluator available\n  %s", strings.Join(reasons, "\n  "))
}

// New returns the named backend, or the default for "".
func New(name string) (Evaluator, error) {
	if name == "" {
		var err error
		if name, err = Default(); err != nil {
			return nil, err
		}
	} else if ok, why := Available(name); !ok {
		return nil, fmt.Errorf("backend %s: %s", name, why)
	}
	switch name {
	case Embedded:
		return embedded{}, nil
	case Local:
		return local{}, nil
	case Docker:
		return docker{}, nil
	}
	return nil, fmt.Errorf("unknown backend %q", name)
}

// EmbeddedVersion names the linked evaluator, or says there is none.
func EmbeddedVersion() string {
	if !iets.Linked {
		return "none (built without the embedded evaluator)"
	}
	return iets.Version()
}
