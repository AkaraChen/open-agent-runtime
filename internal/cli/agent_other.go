//go:build !linux

package cli

import "github.com/spf13/cobra"

func agentCmd() *cobra.Command { return nil }
