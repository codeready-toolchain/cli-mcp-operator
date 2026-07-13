## Why

The session manager (`pkg/session`) can resolve a sandbox pod and proxy commands to the agent, and the typed agent HTTP client (`pkg/agent`) is in place — but nothing yet exposes that capability as an MCP tool. TARSy investigation agents need a standard `tools/call` entry point that accepts a shell command, routes it to the correct session via the `X-Session-ID` header, and returns structured stdout/stderr/exit-code output the LLM can reason over.

SANDBOX-1813 introduces `pkg/tools/bash.go` — the `bash` MCP tool handler. It extracts the session ID from request headers (never from tool params), clamps timeouts, calls `SessionManager.ExecuteCommand`, and maps the agent response to `BashOutput` JSON. Non-zero command exit codes are returned as tool results with `IsError: true` (still carrying output); only infrastructure failures (missing session header, pod creation failure, agent unreachable) surface as handler errors.

SANDBOX-1810 delivered session routing and `ExecuteCommand`. SANDBOX-1812 delivered the agent HTTP client the session manager uses. This story is the MCP-facing glue between those layers and TARSy.

## What Changes

- Add `pkg/tools/bash.go` with:
  - `BashInput` / `BashOutput` types matching the design doc wire contract
  - `CommandExecutor` interface (narrow surface over `SessionManager.ExecuteCommand`) for testability
  - `BashTool` struct holding the executor and MCP tool definition
  - `NewBashTool(executor CommandExecutor)` constructor
  - `RegisterWith(s *mcp.Server)` — registers via `mcp.AddTool` (typed handler, auto schema)
  - Handler logic: header extraction → timeout clamp → execute → map result / errors

- Add `pkg/tools/bash_test.go` with unit tests against a mock `CommandExecutor`

- Add `github.com/modelcontextprotocol/go-sdk` dependency (required for tool registration; not yet in `go.mod`)

## Capabilities

### New Capabilities
- `bash-tool`: MCP `bash` tool that routes shell commands to session-scoped sandbox pods via `X-Session-ID`, returns structured `BashOutput`, and distinguishes command failures (`IsError` + output) from infrastructure failures (handler error)

### Modified Capabilities
<!-- None — this story only adds pkg/tools. Session manager and agent client are unchanged. Wiring into mcp.Server bootstrap is SANDBOX-1814. -->

## Impact

- **Affected code**:
  - `pkg/tools/bash.go` — New file with bash tool handler and registration
  - `pkg/tools/bash_test.go` — New file with unit tests
  - `go.mod` / `go.sum` — Add `github.com/modelcontextprotocol/go-sdk` (aligned with mcp-server-devsandbox at v1.4.x)
- **API changes**: New Go API in `pkg/tools`. The `bash` MCP tool becomes registrable; actual server wiring is SANDBOX-1814.
- **Dependencies**: Depends on SANDBOX-1810 (`SessionManager.ExecuteCommand`) and SANDBOX-1812 (`agent.ExecResponse` types). Depended on by SANDBOX-1814 (MCP server entry point registers the tool).
- **No breaking changes** — entirely new package. Existing session/agent code is not modified.
- **Out of scope**: MCP server bootstrap, HTTP mux, Cobra CLI (SANDBOX-1814); refactoring `manager.go` to use `AgentClient` (optional follow-up noted in SANDBOX-1812); full server-side unit-test suite beyond bash tool tests (SANDBOX-1816 expands coverage).
