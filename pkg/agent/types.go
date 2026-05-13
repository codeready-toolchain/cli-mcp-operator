package agent

// ExecRequest is sent by the MCP server to the sandbox agent to execute a command.
type ExecRequest struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 60
}

// ExecResponse is returned by the sandbox agent after command execution.
type ExecResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

// AssignRequest delivers an HMAC-derived bearer token to a warm-pool pod.
type AssignRequest struct {
	Token string `json:"token"`
}
