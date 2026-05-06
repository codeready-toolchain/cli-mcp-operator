package main

import (
	"fmt"
	"os"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/version"
)

func main() {
	fmt.Fprintf(os.Stderr, "sandbox-agent %s (built %s)\n", version.Commit, version.BuildTime)
	// TODO: HTTP server on :8090, bash session (SANDBOX-1807)
}
