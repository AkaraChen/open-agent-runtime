// Command oar is the host CLI of the composable Linux runtime.
package main

import (
	"os"

	"github.com/AkaraChen/open-agent-runtime/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
