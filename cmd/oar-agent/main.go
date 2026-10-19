// Command oar-agent is the guest agent. Inside the guest VM it is installed
// as /init: when started as PID 1 it initializes the system and supervises a
// second copy of itself running `serve`.
package main

import (
	"fmt"
	"os"

	"github.com/AkaraChen/open-agent-runtime/internal/agent"
)

func main() {
	if os.Getpid() == 1 {
		agent.RunInit()
		return
	}
	if err := agent.Main(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "oar-agent:", err)
		os.Exit(1)
	}
}
