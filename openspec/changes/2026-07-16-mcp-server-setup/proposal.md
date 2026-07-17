## Why

The building blocks for the CLI MCP server control plane are in place: session management and warm pool (`pkg/session`), typed agent HTTP client (`pkg/agent`), and the `bash` MCP tool (`pkg/tools`). Nothing yet wires those pieces into a runnable production service — `cmd/server/main.go` is still a stub, and there is no `pkg/server` package.

SANDBOX-1814 introduces the MCP server bootstrap: Cobra CLI, `mcp.Server` with mcp-common middleware, Streamable HTTP transport, HTTP mux endpoints (`/mcp`, session delete, metrics, liveness, readiness), background cleanup/pool loops, and graceful shutdown on SIGTERM. After this story, TARSy can talk to a real HTTP MCP server that routes `bash` calls into sandbox pods.

## What Changes

- Add `pkg/server/server.go` with:
  - MCP server construction using mcp-common `MetricsMiddleware` + `LoggingMiddleware` (mcp-server-devsandbox pattern)
  - `StreamableHTTPHandler` with `Stateless: true` (HTTP only) and `DisableLocalhostProtection: true` (loopback bind required)
  - HTTP mux: `/mcp`, `DELETE /sessions/{id}` (204), `DELETE /sessions/` (400), `/metrics`, `/live`, `/health`
  - Session delete handler calling `SessionManager.CleanupSession`
  - Health check verifying Kubernetes API reachability (lightweight namespace get)

- Update `cmd/server/main.go` with:
  - Cobra root command and flags (`--address`, `--transport`, `--stateless`, `--namespace`, `--sandbox-image`, `--kubeconfig`, `--hmac-key-file`, `--idle-timeout`, `--warm-pool-size`)
  - Startup validation: image + hmac key required for both transports; HTTP requires `--stateless` and loopback `--address`
  - `runServer()` bootstrap: clientset → SessionManager → mcp.Server + middleware → register bash tool → stale cleanup → warm pool reconciler → HTTP listen → signal **or** serve-error handling
  - Graceful shutdown via `http.Server.Shutdown()` on SIGTERM/SIGINT; fatal return on unexpected ListenAndServe errors

- Add focused unit tests in `pkg/server/server_test.go` (mux endpoints, bash registration, health/delete behavior)

- Add Go dependencies: `mcp-common`, `spf13/cobra`, `prometheus/client_golang`

## Capabilities

### New Capabilities
- `mcp-server-setup`: MCP server wiring — middleware, Streamable HTTP handler options, HTTP mux routes (`/mcp`, session delete, metrics, live, health), and bash tool registration
- `server-entry-point`: Cobra CLI flags, `runServer` lifecycle (K8s client, SessionManager, background loops), and graceful shutdown on SIGTERM/SIGINT

### Modified Capabilities
<!-- None — this story consumes existing packages without changing their public contracts. -->

## Impact

- **Affected code**:
  - `pkg/server/server.go` — New package for MCP/HTTP server setup
  - `pkg/server/server_test.go` — Focused unit tests
  - `cmd/server/main.go` — Replace stub with Cobra + `runServer`
  - `go.mod` / `go.sum` — Add mcp-common, cobra, prometheus client
- **API changes**: New Go API in `pkg/server`. New external HTTP surface on the MCP server process (`/mcp`, `DELETE /sessions/{id}`, `/metrics`, `/live`, `/health`).
- **Dependencies**: Depends on SANDBOX-1813 (`pkg/tools` bash registration), SANDBOX-1810/1811 (`SessionManager`, warm pool), SANDBOX-1812 (agent types used transitively). Depended on by SANDBOX-1815 (server container image) and SANDBOX-1816 (broader server unit tests).
- **No breaking changes** — `cmd/server/main.go` is updated from a placeholder; `pkg/server` is new.
- **Out of scope**: Server container image (SANDBOX-1815); exhaustive `pkg/...` test matrix (SANDBOX-1816); deployment manifests (Phase 3).
