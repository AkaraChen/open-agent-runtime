package vm

import (
	"context"
	"fmt"
)

// android is the extension point for running the guest on Android hosts
// (e.g. via the Android Virtualization Framework / crosvm). It is not part
// of the MVP: the backend is registered so configuration and CLI plumbing
// are already in place.
type android struct{}

func init() { Register(android{}) }

func (android) Name() string          { return "android" }
func (android) NeedsGuestImage() bool { return true }
func (android) NeedsRoot() bool       { return false }
func (android) Available() error {
	return fmt.Errorf("%w: android (AVF/crosvm) is planned after the MVP", ErrUnsupported)
}
func (a android) Boot(ctx context.Context, cfg MachineConfig) (Machine, error) {
	return nil, a.Available()
}
