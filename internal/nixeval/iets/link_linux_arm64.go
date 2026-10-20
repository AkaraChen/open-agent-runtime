//go:build iets

package iets

// #cgo LDFLAGS: -L${SRCDIR}/lib/linux_arm64 -liets_ffi -lm -lpthread -ldl
import "C"
