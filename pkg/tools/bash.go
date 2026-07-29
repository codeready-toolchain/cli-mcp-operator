package tools

import (
	"context"
	"fmt"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/agent"
	"github.com/codeready-toolchain/cli-mcp-server/pkg/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BashInput is the tool input schema for the bash MCP tool.
type BashInput struct {
	Command string `json:"command" jsonschema:"Shell command to execute (full bash — pipes, redirects, chaining supported)"`
	Timeout *int   `json:"timeout,omitempty" jsonschema:"Max execution time in seconds (default 60, max 300)"`
}

// BashOutput is the structured result returned to the LLM.
type BashOutput struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

// CommandExecutor abstracts the session manager's ExecuteCommand for testability.
// *session.SessionManager satisfies this interface without an adapter.
type CommandExecutor interface {
	ExecuteCommand(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error)
}

var (
	defaultTimeout = int(sandbox.DefaultTimeout.Seconds())
	maxTimeout     = int(sandbox.MaxTimeout.Seconds())
)

// BashTool implements the bash MCP tool handler.
type BashTool struct {
	executor CommandExecutor
	tool     *mcp.Tool
}

// NewBashTool creates a BashTool with the given executor.
func NewBashTool(executor CommandExecutor) *BashTool {
	return &BashTool{
		executor: executor,
		tool: &mcp.Tool{
			Name:        "bash",
			Description: "Execute a shell command in a persistent sandbox bash session. Supports full bash syntax: pipes, redirects, chaining, and standard Unix tools.",
		},
	}
}

// RegisterWith registers the bash tool on the given MCP server.
func (t *BashTool) RegisterWith(s *mcp.Server) {
	mcp.AddTool(s, t.tool, t.handle)
}

// Tool returns the underlying mcp.Tool definition.
func (t *BashTool) Tool() *mcp.Tool {
	return t.tool
}

func (t *BashTool) handle(ctx context.Context, req *mcp.CallToolRequest, input BashInput) (*mcp.CallToolResult, BashOutput, error) {
	sessionID, err := extractSessionID(req)
	if err != nil {
		return nil, BashOutput{}, err
	}

	if input.Command == "" {
		return nil, BashOutput{}, fmt.Errorf("command is required")
	}

	timeout := clampTimeout(input.Timeout)

	resp, err := t.executor.ExecuteCommand(ctx, sessionID, input.Command, timeout)
	if err != nil {
		return nil, BashOutput{}, fmt.Errorf("sandbox exec failed: %w", err)
	}

	output := BashOutput{
		Stdout:     resp.Stdout,
		Stderr:     resp.Stderr,
		ExitCode:   resp.ExitCode,
		DurationMs: resp.DurationMs,
	}

	if resp.ExitCode != 0 {
		return &mcp.CallToolResult{IsError: true}, output, nil
	}
	return nil, output, nil
}

func extractSessionID(req *mcp.CallToolRequest) (string, error) {
	if req.Extra == nil || req.Extra.Header == nil {
		return "", fmt.Errorf("missing X-Session-ID header")
	}
	sid := req.Extra.Header.Get("X-Session-ID")
	if sid == "" {
		return "", fmt.Errorf("missing X-Session-ID header")
	}
	return sid, nil
}

func clampTimeout(t *int) int {
	if t == nil || *t <= 0 {
		return defaultTimeout
	}
	if *t > maxTimeout {
		return maxTimeout
	}
	return *t
}
