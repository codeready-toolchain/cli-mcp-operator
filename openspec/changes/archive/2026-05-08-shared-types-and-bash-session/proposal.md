## Why

The CLI MCP Server needs a reliable mechanism for executing arbitrary shell commands inside sandbox pods and capturing their output in a structured way. The MCP server (control plane) and the sandbox agent (data plane) are separate binaries that communicate via HTTP — they need a shared set of request/response types. The sandbox agent needs a persistent bash session that preserves state (environment variables, working directory) between calls, giving the LLM the same investigative experience a developer gets in a terminal.

Without these foundational primitives, the sandbox agent has no way to receive or respond to command requests, and each command would start in a fresh shell — losing the investigative context that makes the agent productive.

## What Changes

- Add `pkg/agent/types.go` with `ExecRequest`, `ExecResponse`, and `AssignRequest` structs defining the shared HTTP contract between server and agent
- Add `pkg/sandbox/bash.go` implementing `BashSession` with:
  - Persistent `bash --norc --noprofile` process managed via stdin/stdout/stderr pipes
  - UUID-based delimiter protocol for isolating command output from the continuous bash stream
  - Concurrent pipe readers using shared `strings.Builder` buffers with `sync.WaitGroup`
  - Timeout handling via SIGKILL to command process group (not bash itself) with 5-second hard deadline
  - Crash recovery: detect EOF on pipe reads, mark session dead, respawn on next Execute() call
  - Eager initialization (bash starts in constructor for fail-fast behavior)
  - Mutex-serialized access for thread safety
- Add `pkg/sandbox/bash.go` domain-specific `ExecResult` type (separate from wire format `ExecResponse`)
- Add `BashConfig` struct for stable constructor signature

## Capabilities

### New Capabilities
- `shared-types`: Defines the HTTP contract types (`ExecRequest`, `ExecResponse`, `AssignRequest`) shared between the MCP server and sandbox agent
- `persistent-bash-session`: Defines the `BashSession` struct and its behavior — command execution, delimiter protocol, timeout handling, crash recovery, and lifecycle management

### Modified Capabilities
<!-- None — these are new foundational components -->

## Impact

- **Affected code**:
  - `pkg/agent/types.go` — New file with shared HTTP contract types
  - `pkg/sandbox/bash.go` — New file with BashSession implementation
- **API changes**: New internal Go API only. No external HTTP or MCP API changes (those are in SANDBOX-1807).
- **Dependencies**: Depends on SANDBOX-1805 (repo scaffold). Depended on by SANDBOX-1807 (agent HTTP handlers), SANDBOX-1809 (agent unit tests), SANDBOX-1812 (agent HTTP client).
- **No breaking changes** — all new code, no modifications to existing components.
