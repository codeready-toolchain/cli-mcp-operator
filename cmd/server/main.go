package main

import (
	"fmt"
	"os"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/version"
)

func main() {
	fmt.Fprintf(os.Stderr, "cli-mcp-server %s (built %s)\n", version.Commit, version.BuildTime)
	// TODO: Cobra root command, MCP server setup (SANDBOX-1814)
}
