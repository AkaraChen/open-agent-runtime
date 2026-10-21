package nixeval

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// nixInstantiateArgs evaluates expr strictly as JSON. Read-write mode lets
// nix copy sources into its store when it computes output paths, as a
// nix-build would.
func nixInstantiateArgs(expr string) []string {
	return []string{"--eval", "--json", "--strict", "--read-write-mode", "--expr", expr}
}

func runNix(ctx context.Context, name string, cmd *exec.Cmd) ([]byte, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nix eval (%s) failed: %w\n%s", name, err, bytes.TrimSpace(stderr.Bytes()))
	}
	_ = ctx
	return out, nil
}

// local runs nix-instantiate from PATH.
type local struct{}

func (local) Name() string { return Local }

func (local) Eval(ctx context.Context, req Request) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nix-instantiate", nixInstantiateArgs(Expression(req.NixpkgsDir, req))...)
	return runNix(ctx, Local, cmd)
}

// docker runs nix-instantiate inside the nixos/nix image, with the checkout
// mounted read-only at /nixpkgs and the image's cache in a named volume.
type docker struct{}

func (docker) Name() string { return Docker }

// DockerImage is the image the docker backend runs.
const DockerImage = "nixos/nix:latest"

func (docker) Eval(ctx context.Context, req Request) ([]byte, error) {
	img := DockerImage
	args := []string{"run", "--rm",
		"-v", req.NixpkgsDir + ":/nixpkgs:ro",
		"-v", "oar-nix-cache:/root/.cache",
		img, "nix-instantiate"}
	args = append(args, nixInstantiateArgs(Expression("/nixpkgs", req))...)
	return runNix(ctx, Docker, exec.CommandContext(ctx, "docker", args...))
}
