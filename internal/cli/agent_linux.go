//go:build linux

package cli

import (
	"github.com/spf13/cobra"

	"github.com/AkaraChen/open-agent-runtime/internal/agent"
)

func agentCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "agent",
		Short:              "Run the guest agent directly on this Linux host",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.Main(args)
		},
	}
}
