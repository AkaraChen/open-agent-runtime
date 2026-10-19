//go:build !linux

package agent

import "fmt"

// Version of the agent.
var Version = "0.1.0"

// RunInit is only meaningful inside a Linux guest.
func RunInit() {}

// Main is unavailable outside Linux; the agent always runs in a Linux guest.
func Main(args []string) error {
	return fmt.Errorf("the guest agent only runs on Linux")
}
