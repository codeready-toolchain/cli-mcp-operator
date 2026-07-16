## Context

The CLI MCP Server has two binaries: the MCP server (control plane) and the sandbox agent (data plane). Prior stories delivered session management, warm pool, agent HTTP client, and the `bash` MCP tool. SANDBOX-1814 wires those into a production-ready HTTP service following the `mcp-server-devsandbox/pkg/mcpinit` pattern, as specified in `docs/proposals/cli-mcp-server-design.md` and `docs/implementation/implementation-plan.md` §§2.6–2.7.

The server runs behind kube-rbac-proxy in production. TARSy calls `/mcp` with `X-Session-ID`, and calls `DELETE /sessions/{id}` when an investigation ends. Kubernetes probes `/live` and `/health`; Prometheus scrapes `/metrics`.

## Goals / Non-Goals

**Goals:**
- Construct `mcp.Server` with mcp-common `MetricsMiddleware` + `LoggingMiddleware`
- Serve MCP over Streamable HTTP with `Stateless: true` and `DisableLocalhostProtection: true`
- HTTP mux: `/mcp`, `DELETE /sessions/{id}` (204), `/metrics`, `/live`, `/health` (K8s API reachability)
- Cobra CLI with the documented flags (including `--transport` / `--stateless` for mcp-server-devsandbox parity)
- Bootstrap order: clientset → SessionManager → mcp.Server + middleware → register bash → stale cleanup → warm pool (if size > 0) → listen → signal handling
- Graceful shutdown on SIGTERM/SIGINT via `http.Server.Shutdown()`
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

### Decision 2: StreamableHTTP with Stateless + DisableLocalhostProtection

```go
mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
    return server
}, &mcp.StreamableHTTPOptions{
    Stateless:                  true, // production default for HTTP; flag may override for local
    DisableLocalhostProtection: true,
})
```

For production HTTP deployment the design requires `Stateless: true`. The Cobra `--stateless` flag controls the value passed into options (default `false` for local/stdio parity with mcp-server-devsandbox; deployment manifests pass `--stateless`).

**Rationale:**
- Stateless mode enables multi-replica load balancing without sticky sessions
- kube-rbac-proxy forwards with the external `Host` header to a localhost listener; DNS-rebinding protection would reject those requests without `DisableLocalhostProtection`

**Alternative considered:** Always hard-code `Stateless: true` and drop the flag
- Rejected — breaks local stdio/dev parity with mcp-server-devsandbox; design doc includes the flag

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

`DELETE /sessions/{id}` calls `SessionManager.CleanupSession` and returns **204 No Content** on success. Missing ID → 400. Cleanup failure → 500 with error text.

Use Go 1.22+ method routing: `mux.HandleFunc("DELETE /sessions/{id}", ...)`.

**Rationale:**
- Design doc Q2: lifecycle is TARSy's job; MCP tools stay investigation-focused
- Same session ID TARSy already injects as `X-Session-ID`
- Method-based routing matches the agent mux and `use-modern-go`

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

### Decision 6: Prefer `http.Server` + `Shutdown()` over bare `ListenAndServe`

```go
srv := &http.Server{Addr: address, Handler: mux}
// on SIGTERM/SIGINT:
shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
defer cancel()
_ = srv.Shutdown(shutdownCtx)
```

Cancel the background context that drives stale cleanup and warm-pool reconciler so those loops stop during shutdown.

**Rationale:**
- Agent entry point already uses `Shutdown()`; mcpinit's bare `ListenAndServe` is weaker
- Drains in-flight MCP/`DELETE` requests instead of hard-killing them
- Shutdown timeout should accommodate long bash commands (align with agent: 310s, or document a server-side choice ≥ max tool timeout)

**Alternative considered:** Copy mcpinit's `ListenAndServe` + log-and-exit
- Rejected — loses in-flight request drain; worse than the agent pattern we already ship

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
| `--address` / `-a` | `localhost:8080` | HTTP listen address |
| `--transport` / `-t` | `stdio` | stdio or http |
| `--stateless` | `false` | StreamableHTTPOptions.Stateless + Tools.ListChanged |
| `--namespace` | `tarsy` | `SandboxConfig.Namespace` |
| `--sandbox-image` | _(required for HTTP run)_ | `SandboxConfig.Image` |
| `--kubeconfig` | `""` | path for sandbox pods' kubeconfig secret source / local client override as implemented |
| `--hmac-key-file` | _(required)_ | read file → `SandboxConfig.HMACKey` |
| `--idle-timeout` | `30m` | `SandboxConfig.IdleTimeout` |
| `--warm-pool-size` | `0` | `SandboxConfig.WarmPoolSize` |

`--sandbox-image` and `--hmac-key-file` must be non-empty before constructing `SessionManager` (manager already validates Image/HMACKey).

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
- **Stateless default false:** Operators must pass `--stateless` in manifests; forgetting it in multi-replica deploys can cause notification issues. Mitigated by documenting in tasks and matching mcp-server-devsandbox defaults.
- **Health only checks API:** A broken image/pull can still look "ready"; acceptable per design (failures surface on first `bash` call).

## Migration Plan

None — new runtime surface. Existing agent binary and packages are unchanged.
