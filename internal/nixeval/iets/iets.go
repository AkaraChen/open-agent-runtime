//go:build iets

// Package iets binds the iets Nix evaluator, linked as a static library
// built from rust/iets-ffi. Without the `iets` build tag the package is a
// stub and Linked is false.
package iets

/*
#cgo CFLAGS: -I${SRCDIR}/../../../rust/iets-ffi/include
#include <stdlib.h>
#include "iets_ffi.h"
*/
import "C"

import (
	"fmt"
	"strings"
	"unsafe"
)

// Linked is true when the evaluator is part of this binary.
const Linked = true

// Version is the evaluator's own version string.
func Version() string { return C.GoString(C.oar_iets_version()) }

// Error is a failed evaluation: the exit code the iets command line would
// have used, and the report it would have printed.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

func cstr(s string) *C.char {
	if s == "" {
		return nil
	}
	return C.CString(s)
}

func free(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

// Eval evaluates expr strictly for system and returns its JSON. projectRoot,
// when set, makes the evaluation pure and limits what it may read to that
// directory. cacheDir holds the compiled-file cache; workers is the thread
// count, 0 for the default.
func Eval(expr, system, projectRoot, cacheDir string, workers int) ([]byte, error) {
	cExpr, cSystem, cRoot, cCache := C.CString(expr), C.CString(system), cstr(projectRoot), cstr(cacheDir)
	defer free(cExpr)
	defer free(cSystem)
	defer free(cRoot)
	defer free(cCache)
	var jsonOut, errOut *C.char
	if workers < 0 {
		workers = 0
	}
	code := C.oar_iets_eval(cExpr, cSystem, cRoot, cCache, C.uint32_t(workers), &jsonOut, &errOut)
	defer C.oar_iets_free(jsonOut)
	defer C.oar_iets_free(errOut)
	if code != 0 {
		msg := strings.TrimSpace(C.GoString(errOut))
		if msg == "" {
			msg = fmt.Sprintf("evaluator failed with code %d", int(code))
		}
		return nil, &Error{Code: int(code), Message: msg}
	}
	return []byte(C.GoString(jsonOut)), nil
}
