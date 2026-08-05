## Why

The session manager (`pkg/session/manager.go`) and warm pool (`pkg/session/pool.go`) both make HTTP calls to the sandbox agent — `POST /exec` with a bearer token, `POST /assign` with a token payload, and readiness checks via `GET /health`. Today each call site constructs `http.Request` objects inline, marshals JSON manually, checks status codes, and decodes responses with duplicated error handling. This makes the agent HTTP contract hard to reason about and easy to break in subtle ways (e.g., one call site invalidates the cache on transport errors while another doesn't, or a new endpoint is added without consistent timeout handling).

SANDBOX-1812 introduces `pkg/agent/client.go` — a typed HTTP client that encapsulates all agent communication behind three methods: `Execute`, `Assign`, and `HealthCheck`. The client owns JSON encoding/decoding, bearer token injection, timeout enforcement, and a structured error type hierarchy that distinguishes network failures from non-200 responses from JSON decode errors. Once in place, the session manager and warm pool can replace their inline HTTP code with single-method calls, and the downstream bash tool handler (SANDBOX-1813) gets a clean interface to build on.

SANDBOX-1806 delivered the shared types (`ExecRequest`, `ExecResponse`, `AssignRequest`) that define the wire contract. This story builds the Go HTTP client that speaks that contract.

## What Changes

- Add `pkg/agent/client.go` with:
  - `AgentClient` struct holding an `*http.Client`, default timeout, and agent port
  - `NewAgentClient(opts ...Option)` constructor with functional options for timeout, HTTP client, and port
  - `Execute(ctx, podIP, token, req)` — POST `/exec` with bearer auth, returns `*ExecResponse` or typed error
  - `Assign(ctx, podIP, req)` — POST `/assign` with JSON body, returns typed error
  - `HealthCheck(ctx, podIP)` — GET `/health`, returns typed error

- Add `pkg/agent/errors.go` with:
  - `AgentError` base type with `Kind` enum (`NetworkError`, `StatusError`, `DecodeError`)
  - `NetworkError` — wraps transport-level failures (connection refused, timeout, DNS)
  - `StatusError` — non-200 HTTP status with status code and optional body
  - `DecodeError` — JSON decode failure with the raw body that couldn't be parsed
  - All error types implement `error` and `Unwrap()` for `errors.Is`/`errors.As` compatibility

- Add `pkg/agent/client_test.go` with tests against `httptest.NewServer`

## Capabilities

### New Capabilities
- `agent-client`: Typed HTTP client for the sandbox agent API (`/exec`, `/assign`, `/health`) with structured error handling and configurable timeouts

### Modified Capabilities
<!-- None — existing code is not modified in this story. The session manager and warm pool will be refactored to use AgentClient in a follow-up or as part of SANDBOX-1813 integration. -->

## Impact

- **Affected code**:
  - `pkg/agent/client.go` — New file with AgentClient
  - `pkg/agent/errors.go` — New file with typed error types
  - `pkg/agent/client_test.go` — New file with unit tests
- **API changes**: New Go API in `pkg/agent` package. No external HTTP endpoints added.
- **Dependencies**: Depends on SANDBOX-1806 (`pkg/agent/types.go` — shared request/response types). Depended on by SANDBOX-1813 (bash tool handler).
- **No new Go dependencies** — uses `net/http`, `encoding/json`, `context`, `fmt`, `errors` from stdlib.
- **No breaking changes** — entirely new files. Existing inline HTTP code in `manager.go` and `pool.go` continues to work and will be refactored separately.
