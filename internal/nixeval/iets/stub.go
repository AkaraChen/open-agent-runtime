//go:build !iets

// Package iets binds the iets Nix evaluator. This build has no library
// linked: build with `-tags iets` after hack/build-iets.sh.
package iets

import "errors"

// Linked is true when the evaluator is part of this binary.
const Linked = false

// ErrNotLinked is every call's answer in this build.
var ErrNotLinked = errors.New("the embedded nix evaluator is not linked into this binary")

// Version is the evaluator's own version string.
func Version() string { return "" }

// Eval evaluates expr; see the cgo build.
func Eval(expr, system, projectRoot, cacheDir string, workers int) ([]byte, error) {
	return nil, ErrNotLinked
}
