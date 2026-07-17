## ADDED Requirements

### Requirement: MCP server is constructed with mcp-common middleware

The server package SHALL construct an `mcp.Server` following the mcp-server-devsandbox pattern: JSON slog logger, `ServerCapabilities.Tools.ListChanged` equal to `!stateless`, and receiving middleware `MetricsMiddleware` then `LoggingMiddleware` from mcp-common.

#### Scenario: Middleware order and capabilities in stateful mode
- **WHEN** the MCP server is created with `stateless == false` (stdio only)
- **THEN** `Tools.ListChanged` SHALL be true
- **AND** MetricsMiddleware and LoggingMiddleware SHALL be registered as receiving middleware

#### Scenario: ListChanged disabled in stateless mode
- **WHEN** the MCP server is created with `stateless == true` (required for HTTP)
- **THEN** `Tools.ListChanged` SHALL be false

### Requirement: Streamable HTTP handler uses production-safe options

When serving HTTP, the `/mcp` endpoint SHALL use `mcp.NewStreamableHTTPHandler` with `Stateless: true` and `DisableLocalhostProtection: true`. Stateful HTTP is not supported.

#### Scenario: Localhost protection disabled
- **WHEN** the Streamable HTTP handler is constructed
- **THEN** `DisableLocalhostProtection` SHALL be true

#### Scenario: HTTP always uses Stateless true
- **WHEN** the server serves HTTP transport
- **THEN** `StreamableHTTPOptions.Stateless` SHALL be true
- **AND** `Tools.ListChanged` SHALL be false

### Requirement: DisableLocalhostProtection requires a loopback bind address

HTTP listen addresses SHALL use a loopback host so `/mcp` and `DELETE /sessions/{id}` are not exposed without the kube-rbac-proxy authentication boundary.

#### Scenario: Loopback address accepted
- **WHEN** `--transport` is `http` and `--address` host is `127.0.0.1`, `localhost`, or `::1`
- **THEN** startup SHALL proceed (subject to other validation)

#### Scenario: Non-loopback address rejected
- **WHEN** `--transport` is `http` and `--address` host is not loopback
- **THEN** `runServer` SHALL return a clear error before listening
- **AND** it SHALL NOT start the HTTP server

### Requirement: HTTP mux exposes MCP, session delete, metrics, live, and health

The HTTP mux SHALL register these operational routes: `/mcp`, `DELETE /sessions/{id}`, `DELETE /sessions/` (empty-id fallback), `/metrics`, `/live`, `/health`.

#### Scenario: MCP endpoint mounted
- **WHEN** the HTTP mux is built for HTTP transport
- **THEN** requests to `/mcp` SHALL be handled by the Streamable HTTP handler

#### Scenario: Metrics endpoint
- **WHEN** a client GETs `/metrics`
- **THEN** the response SHALL be served by Prometheus `promhttp.Handler()`

### Requirement: DELETE /sessions/{id} cleans up and returns 204

Session cleanup SHALL be a plain HTTP DELETE endpoint (not an MCP tool). On success it SHALL return 204 No Content.

#### Scenario: Successful session delete
- **WHEN** a client sends `DELETE /sessions/{id}` with a non-empty `{id}`
- **AND** `CleanupSession` succeeds
- **THEN** the handler SHALL return HTTP 204 with an empty body

#### Scenario: Missing session id via empty-path fallback
- **WHEN** a client sends `DELETE /sessions/`
- **THEN** the mux SHALL return HTTP 400 (not 404 and not a redirect)
- **AND** it SHALL NOT call `CleanupSession`

#### Scenario: Empty path value on parameterized route
- **WHEN** the `{id}` path value is empty after routing
- **THEN** the handler SHALL return HTTP 400
- **AND** it SHALL NOT call `CleanupSession`

#### Scenario: Cleanup failure
- **WHEN** `CleanupSession` returns an error
- **THEN** the handler SHALL return HTTP 500
- **AND** the response body SHALL include an error description

#### Scenario: Method-based routing
- **WHEN** the mux is configured
- **THEN** it SHALL register `DELETE /sessions/{id}` for non-empty ids
- **AND** it SHALL register `DELETE /sessions/` as an empty-id fallback returning 400

### Requirement: Liveness probe does not depend on external systems

`/live` SHALL report process liveness without calling Kubernetes or sandbox agents.

#### Scenario: Live always succeeds when process serves HTTP
- **WHEN** a client GETs `/live`
- **THEN** the handler SHALL return HTTP 200
- **AND** the JSON body SHALL include `"status":"alive"`

### Requirement: Readiness probe verifies Kubernetes API reachability

`/health` SHALL verify the server can reach the Kubernetes API with a short timeout (≤ 5 seconds) via a lightweight check (namespace get for the configured namespace). It SHALL NOT probe individual sandbox pods.

#### Scenario: Healthy when API reachable
- **WHEN** a client GETs `/health`
- **AND** the Kubernetes API check succeeds
- **THEN** the handler SHALL return HTTP 200
- **AND** the JSON body SHALL include `"status":"healthy"`

#### Scenario: Unhealthy when API unreachable
- **WHEN** a client GETs `/health`
- **AND** the Kubernetes API check fails
- **THEN** the handler SHALL return HTTP 503
- **AND** the JSON body SHALL include `"status":"unhealthy"` and an `error` field

### Requirement: bash tool is registered on the MCP server

The bootstrap wiring SHALL register the existing `bash` tool from `pkg/tools` on the constructed `mcp.Server` using `NewBashTool(executor).RegisterWith(server)` where the executor is the `SessionManager`.

#### Scenario: bash tool available after setup
- **WHEN** server setup completes tool registration
- **THEN** the MCP server SHALL expose a tool named `"bash"`
