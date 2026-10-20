package nixeval

import (
	"context"
	"path/filepath"

	"github.com/AkaraChen/open-agent-runtime/internal/nixeval/iets"
)

// embedded evaluates with the iets library linked into this binary. The
// checkout is the project root of a pure evaluation, so nothing outside it
// is read. A running evaluation cannot be cancelled; the nix-eval process
// it runs in can be killed.
type embedded struct{}

func (embedded) Name() string { return Embedded }

func (embedded) Eval(_ context.Context, req Request) ([]byte, error) {
	// iets compares paths textually against the project root, so hand it
	// the resolved one and import by that same path.
	dir := req.NixpkgsDir
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return iets.Eval(Expression(dir, req), req.System, dir, req.CacheDir, req.Workers)
}
