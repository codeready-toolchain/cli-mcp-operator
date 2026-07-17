## Context

The CLI MCP Server has two binaries: the MCP server (control plane) and the sandbox agent (data plane). Prior stories delivered session management, warm pool, agent HTTP client, and the `bash` MCP tool. SANDBOX-1814 wires those into a production-ready HTTP service following the `mcp-server-devsandbox/pkg/mcpinit` pattern, as specified in `docs/proposals/cli-mcp-server-design.md` and `docs/implementation/implementation-plan.md` §§2.6–2.7.

The server runs behind kube-rbac-proxy in production and MUST bind to a loopback address (`127.0.0.1` / `localhost` / `::1`). TARSy calls `/mcp` with `X-Session-ID`, and calls `DELETE /sessions/{id}` when an investigation ends. Kubernetes probes `/live` and `/health`; Prometheus scrapes `/metrics`. Non-loopback binds that expose `/mcp` or `DELETE /sessions/{id}` without an authentication boundary are rejected at startup.

## Goals / Non-Goals

**Goals:**
- Construct `mcp.Server` with mcp-common `MetricsMiddleware` + `LoggingMiddleware`
- Serve MCP over Streamable HTTP with `Stateless: true` and `DisableLocalhostProtection: true` (loopback bind required)
- HTTP mux: `/mcp`, `DELETE /sessions/{id}` (204), `DELETE /sessions/` (400), `/metrics`, `/live`, `/health` (K8s API reachability)
- Cobra CLI with documented flags; HTTP requires `--stateless` + loopback `--address`; image + hmac key required for both transports
- Bootstrap order: validate flags → clientset → SessionManager → mcp.Server + middleware → register bash → stale cleanup → warm pool (if size > 0) → listen → signal or serve-error handling
- Graceful shutdown on SIGTERM/SIGINT via `http.Server.Shutdown()`; fatal exit on unexpected ListenAndServe errors
- Focused unit tests for mux/handlers and bash registration

**Non-Goals:**
- Server container image (`Containerfile.server`) — SANDBOX-1815
- Exhaustive unit-test matrix for all packages — SANDBOX-1816
- Kustomize / RBAC / NetworkPolicy deployment — Phase 3
- Refactoring `SessionManager` to use `AgentClient` (optional follow-up)
- stdio transport as a primary production path (supported for local use; HTTP + stateless is the deployment mode)

## Decisions

### Decision 1: Follow mcp-server-devsandbox `mcpinit` for server + middleware

Construct the server like `mcp-server-devsandbox/pkg/mcpinit/init.go` `setupServer`:

```go
srv := mcp.NewServer(
    &mcp.Implementation{Name: "cli-mcp-server", Version: version.Commit},
    &mcp.ServerOptions{
        Capabilities: &mcp.ServerCapabilities{
            Tools: &mcp.ToolCapabilities{ListChanged: !stateless},
        },
        Logger: logger,
    })
srv.AddReceivingMiddleware(middleware.NewMetricsMiddleware(serverName, logger))
srv.AddReceivingMiddleware(middleware.NewLoggingMiddleware(logger))
```

**Rationale:**
- `.coderabbit.yaml` and the design doc explicitly require this pattern
- Reuses battle-tested mcp-common middleware instead of reinventing metrics/logging
- `ListChanged: !stateless` avoids notifications that cannot be delivered in multi-replica mode

**Alternative considered:** Custom middleware in this repo
- Rejected — would duplicate mcp-common and diverge from sibling servers

### Decision 2: StreamableHTTP — HTTP forces Stateless; DisableLocalhostProtection requires loopback

```go
mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
    return server
}, &mcp.StreamableHTTPOptions{
    Stateless:                  true, // always for HTTP transport
    DisableLocalhostProtection: true, // only safe with loopback bind + trusted proxy
})
```

**Stateless contract (resolved):**
- `--transport http` **requires** `--stateless` (startup error if false). HTTP always sets `StreamableHTTPOptions.Stateless: true` and `Tools.ListChanged: false`.
- `--transport stdio` **rejects** `--stateless` (startup error if true), matching mcp-server-devsandbox.
- There is no supported “stateful HTTP” mode — avoids accidental multi-replica misconfiguration.

**Trust boundary for `DisableLocalhostProtection`:**
- Always `true` for HTTP (kube-rbac-proxy forwards with an external `Host` header to a localhost listener).
- `--address` for HTTP MUST resolve to a loopback host (`127.0.0.1`, `localhost`, `::1`). Non-loopback addresses are rejected at startup with a clear error.
- Production manifests bind `127.0.0.1:8080` behind kube-rbac-proxy; `/mcp` and `DELETE /sessions/{id}` MUST NOT be exposed on a non-loopback address without an authentication boundary.

**Rationale:**
- Stateless HTTP is required for multi-replica load balancing (parent design + AC)
- Forcing/requiring `--stateless` for HTTP removes the conflicting “default false / production true” contract
- Loopback-only bind keeps `DisableLocalhostProtection` inside the intended trust model

**Alternative considered:** Keep configurable stateful HTTP via `--stateless=false`
- Rejected — conflicts with AC and multi-replica deployment; easy to misconfigure

**Alternative considered:** Allow non-loopback with a bypass flag
- Rejected for this story — YAGNI; NetworkPolicy + kube-rbac-proxy + loopback is the deployment model

### Decision 3: Split `pkg/server` (wiring) from `cmd/server` (CLI entry)

- `pkg/server` owns mcp.Server construction, mux, and HTTP handlers (testable without Cobra)
- `cmd/server` owns flags, clientset construction, SessionManager lifecycle, and process signals

**Rationale:**
- Matches design doc layout (`pkg/server/server.go` + `cmd/server/main.go`)
- Unit tests hit mux/handlers with fakes; no need to execute Cobra in most tests
- Keeps `main` thin

**Alternative considered:** Everything in `cmd/server/main.go`
- Rejected — harder to unit-test handlers; diverges from design doc

### Decision 4: Session delete is plain HTTP, not an MCP tool

`DELETE /sessions/{id}` calls `SessionManager.CleanupSession` and returns **204 No Content** on success. Cleanup failure → 500 with error text.

Use Go 1.22+ method routing for the primary route, **plus** an explicit empty-id fallback:

```go
mux.HandleFunc("DELETE /sessions/{id}", sessionDeleteHandler) // non-empty id
mux.HandleFunc("DELETE /sessions/", emptySessionIDHandler)    // 400, no CleanupSession
```

Go’s ServeMux does not invoke `{id}` handlers for `DELETE /sessions/` (returns 404) and may redirect `DELETE /sessions//`. The fallback ensures missing IDs return **400** without calling `CleanupSession`, instead of 404/redirect.

**Rationale:**
- Design doc Q2: lifecycle is TARSy's job; MCP tools stay investigation-focused
- Same session ID TARSy already injects as `X-Session-ID`
- Method-based routing matches the agent mux and `use-modern-go`
- Explicit `/sessions/` route matches the AC “missing id → 400” behavior under Go 1.22+ mux rules

**Alternative considered:** MCP `session_end` tool
- Rejected — already decided against in the parent design; LLM should not manage lifecycle

### Decision 5: `/live` vs `/health` semantics

| Endpoint | Meaning | Dependencies |
|---|---|---|
| `/live` | Process responsive | None — always 200 JSON `{"status":"alive"}` when the handler runs |
| `/health` | Ready for traffic | Lightweight K8s API check (namespace get for configured `--namespace`); 503 + error JSON when unreachable |

**Rationale:**
- Matches mcp-server-devsandbox probe split
- Design doc: health does **not** check individual sandbox pods (those are lazy on request)
- Prevents routing traffic to a pod that cannot create/manage sandboxes

**Alternative considered:** Health also probes warm-pool pods
- Rejected — slow, flaky, and out of scope for readiness; pool failures are handled at request time

### Decision 6: Prefer `http.Server` + `Shutdown()`; observe serve errors

```go
srv := &http.Server{Addr: address, Handler: mux}
serverErrChan := make(chan error, 1)
go func() {
    err := srv.ListenAndServe()
    if err != nil && err != http.ErrServerClosed {
        serverErrChan <- err
    }
}()

select {
case <-sigCtx.Done(): // SIGTERM/SIGINT
case err := <-serverErrChan:
    // fatal serve failure (e.g. address in use)
}
// cancel background ctx; Shutdown with timeout
```

Cancel the background context that drives stale cleanup and warm-pool reconciler on **either** signal or unexpected serve failure. Treat `http.ErrServerClosed` as normal shutdown; any other `ListenAndServe` error is fatal and must stop workers and return.

**Rationale:**
- Agent entry point already uses `Shutdown()`; also adopts mcpinit’s `serverErrChan` pattern so a failed listener cannot leave cleanup/pool goroutines running with no HTTP service
- Drains in-flight MCP/`DELETE` requests on signal
- Shutdown timeout should accommodate long bash commands (310s = max bash 300s + 10s buffer)

**Alternative considered:** Wait only on signals, ignore serve errors
- Rejected — address-in-use or bind failures would leave a zombie process with background workers

### Decision 7: Stale cleanup and warm pool started from `runServer`

After SessionManager construction:
1. Start a goroutine that periodically calls `CleanupStale` (every 5 minutes per design doc), cancelled on shutdown context
2. If `WarmPoolSize > 0`, call `SessionManager.StartPool(ctx)` (existing API)

**Rationale:**
- Design/`runServer` step list requires both
- Session manager already owns pool reconciler; entry point only supplies a cancellable context
- Idle timeout comes from `--idle-timeout` into `SandboxConfig.IdleTimeout`

### Decision 8: Required flags and config mapping

| Flag | Default | Maps to |
|---|---|---|
| `--address` / `-a` | `localhost:8080` | HTTP listen address (must be loopback for HTTP) |
| `--transport` / `-t` | `stdio` | stdio or http |
| `--stateless` | `false` | Required `true` for HTTP; forbidden for stdio |
| `--namespace` | `tarsy` | `SandboxConfig.Namespace` |
| `--sandbox-image` | `""` | `SandboxConfig.Image` — **required for both transports** |
| `--kubeconfig` | `""` | local client-go override when not in-cluster |
| `--hmac-key-file` | `""` | read file → `SandboxConfig.HMACKey` — **required for both transports** |
| `--idle-timeout` | `30m` | `SandboxConfig.IdleTimeout` |
| `--warm-pool-size` | `0` | `SandboxConfig.WarmPoolSize` |

`runServer` always constructs `SessionManager`, so `--sandbox-image` and a readable `--hmac-key-file` are required for **both** `stdio` and `http`. A bare `cli-mcp-server` with only defaults will fail validation early (before listen) — that is intentional; there is no “zero-flag” production start.

**Startup validation summary:**
1. `--sandbox-image` non-empty
2. `--hmac-key-file` readable and non-empty contents
3. `http` ⇒ `--stateless` must be true; address host must be loopback
4. `stdio` ⇒ `--stateless` must be false

**Kubeconfig / clientset:** Prefer in-cluster config when running in-cluster; support `--kubeconfig` for local development client construction (standard client-go loading rules). The sandbox pods' mounted kubeconfig secret name remains the SessionManager default (`cli-mcp-investigation-kubeconfig`) unless later stories add a flag.

### Decision 9: Register bash via existing `pkg/tools` API

```go
tools.NewBashTool(sessionManager).RegisterWith(server)
```

`*session.SessionManager` already satisfies `tools.CommandExecutor`.

**Rationale:**
- SANDBOX-1813 designed this exact wiring point
- No adapter type needed

## Risks / Trade-offs

- **Long shutdown vs K8s grace period:** Server shutdown timeout must stay coordinated with Deployment `terminationGracePeriodSeconds` (Phase 3). Spec documents 310s to match agent/max bash timeout.
- **HTTP requires explicit `--stateless`:** Forgetting the flag fails startup instead of silently running stateful HTTP — preferred failure mode for multi-replica safety.
- **Loopback-only HTTP bind:** Blocks casual non-loopback exposure when `DisableLocalhostProtection` is on; Phase 3 manifests must keep the kube-rbac-proxy → `127.0.0.1` pattern.
- **Health only checks API:** A broken image/pull can still look "ready"; acceptable per design (failures surface on first `bash` call).

## Migration Plan

None — new runtime surface. Existing agent binary and packages are unchanged.
