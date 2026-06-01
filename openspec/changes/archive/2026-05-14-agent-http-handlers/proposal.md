## Why

The sandbox agent has a working persistent bash session (SANDBOX-1806) but no way to receive commands from the network. The MCP server needs to send commands to the agent over HTTP, deliver auth tokens to warm-pool pods, and probe health for Kubernetes readiness. Without HTTP handlers and an entry point, the bash session is a library with no runtime — nothing starts it, nothing talks to it, nothing shuts it down.

## What Changes

- Add `pkg/sandbox/handler.go` with:
  - `AgentState` struct (mutex-protected) tracking unassigned/assigned state and bearer token
  - `Handler` struct with method handlers for three HTTP endpoints
  - `HandleExec` — bearer token auth via `hmac.Equal()`, parses `ExecRequest`, calls `BashSession.Execute()`, maps `ExecResult` to `ExecResponse`
  - `HandleAssign` — one-time token delivery for warm-pool pods, transitions state from unassigned to assigned
  - `HandleHealth` — unauthenticated readiness probe, returns 200 if bash alive, 503 if dead
  - `ErrorResponse` type and `writeError` helper for consistent JSON error responses
- Update `cmd/agent/main.go` with:
  - `SANDBOX_AUTH_TOKEN` env var reading for initial state
  - `BashSession` creation with eager initialization
  - HTTP mux with Go 1.22+ method-based routing (`POST /exec`, `POST /assign`, `GET /health`)
  - `net/http.Server` on `:8090` with graceful shutdown via `Shutdown()` (310s timeout)
  - Signal handling for `SIGTERM`/`SIGINT` via `signal.NotifyContext()`

## Capabilities

### New Capabilities
- `agent-http-handlers`: Defines the HTTP endpoints (`POST /exec`, `POST /assign`, `GET /health`), authentication via bearer token with constant-time comparison, agent state machine (unassigned → assigned), and error response format
- `agent-entry-point`: Defines the agent binary entry point — HTTP server lifecycle, signal handling, graceful shutdown with 310s timeout coordinated with Kubernetes `terminationGracePeriodSeconds`

### Modified Capabilities
<!-- None — these build on SANDBOX-1806 without modifying it -->

## Impact

- **Affected code**:
  - `pkg/sandbox/handler.go` — New file with AgentState, Handler, and HTTP endpoint implementations
  - `cmd/agent/main.go` — Updated from scaffold placeholder to full agent entry point
- **API changes**: New HTTP API exposed on port 8090 (POST /exec, POST /assign, GET /health). No changes to existing Go APIs — `BashSession` and shared types from SANDBOX-1806 are consumed as-is.
- **Dependencies**: Depends on SANDBOX-1806 (shared types + BashSession). Depended on by SANDBOX-1808 (agent container image), SANDBOX-1809 (agent unit tests), SANDBOX-1812 (agent HTTP client).
- **No breaking changes** — `cmd/agent/main.go` is updated from a placeholder, all other code is new.
