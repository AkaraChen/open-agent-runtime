//go:build iets

package iets

// #cgo LDFLAGS: -L${SRCDIR}/lib/darwin_amd64 -liets_ffi -liconv -framework Security -framework CoreFoundation -framework SystemConfiguration
import "C"
