## Context

The CLI MCP Server communicates with sandbox agents over HTTP within the Kubernetes pod network. Two existing call sites — `SessionManager.ExecuteCommand` in `manager.go` and `WarmPool.assignToken` in `pool.go` — construct HTTP requests inline, each with slightly different error handling patterns. The session manager invalidates its pod cache on transport errors; the warm pool does not (it rolls back the entire claim). Neither call site distinguishes between "agent returned 500" and "agent's response was garbage JSON" — both are opaque `fmt.Errorf` strings.

SANDBOX-1813 (bash tool handler) needs to call `Execute` with clean error semantics: transport errors should trigger cache invalidation, status errors should be reported with the HTTP code, and decode errors should surface the malformed body for debugging. Building this into each call site would mean triplicate error classification logic.

`AgentClient` centralizes agent HTTP communication in one place with typed errors, configurable timeouts, and a consistent API surface. The session manager and warm pool will be refactored to use it (reducing their inline HTTP code to single-method calls), and the bash tool handler will depend on it directly.

## Goals / Non-Goals

**Goals:**
- `AgentClient` struct with `Execute`, `Assign`, `HealthCheck` methods
- Functional options pattern for configuration (timeout, HTTP client, agent port)
- Typed error hierarchy: `*NetworkError`, `*StatusError`, `*DecodeError` — each implementing `error` and `Unwrap()`
- Thread-safe: `AgentClient` holds only immutable config and a shared `*http.Client` (which is documented as safe for concurrent use). Multiple goroutines can call `Execute`, `Assign`, `HealthCheck` concurrently without external synchronization.
- Per-request context support: all methods accept `context.Context` for cancellation and deadline propagation
- Trust boundary documentation: HTTP (not HTTPS) within cluster, with rationale

**Non-Goals:**
- Retry logic (callers decide whether/how to retry based on error type)
- Circuit breaker or connection pooling configuration (stdlib defaults are sufficient)
- Refactoring `manager.go` and `pool.go` to use `AgentClient` (separate change, reduces risk)
- Metrics or tracing instrumentation (SANDBOX-1820)
- Response body size limits (agent responses are bounded by command output; if needed, add later)

## Decisions

### Decision 1: Separate `AgentClient` struct in `pkg/agent/` alongside `types.go`

Place the client in the same package as the shared types it uses (`ExecRequest`, `ExecResponse`, `AssignRequest`).

**Rationale:**
- The `pkg/agent` package is already the home for the agent wire contract (`types.go`). Adding the client here keeps all agent-facing code together.
- Avoids a circular dependency — `pkg/session` imports `pkg/agent`, not the other way around
- The client is a pure HTTP wrapper over the types defined in the same package — they are a natural unit

**Alternative considered:** `pkg/agent/client/` sub-package
- Rejected — introduces an extra package for a single struct; the `pkg/agent` package is small (one file today) and benefits from co-location

### Decision 2: Functional options for `NewAgentClient`

```go
type Option func(*AgentClient)

func WithTimeout(d time.Duration) Option
func WithHTTPClient(c *http.Client) Option
func WithPort(port int) Option
```

**Rationale:**
- Follows idiomatic Go patterns for optional configuration
- Defaults are sensible (30s timeout, `http.DefaultClient`-like behavior, port 8090)
- Easy to extend with new options (e.g., `WithLogger`) without breaking existing callers
- Testing injects a custom `*http.Client` via `WithHTTPClient`

**Alternative considered:** Config struct parameter
- Acceptable but less ergonomic for the common case where defaults suffice. Functional options are the established pattern in this codebase (see `mcp-common` middleware options).

### Decision 3: Typed error hierarchy with `Kind` enum

Three concrete error types, each carrying domain-relevant context:

```go
type NetworkError struct {
    Op  string    // "execute", "assign", "health_check"
    URL string
    Err error     // underlying net error
}

type StatusError struct {
    Op         string
    URL        string
    StatusCode int
    Body       string  // first 512 bytes of response body for debugging
}

type DecodeError struct {
    Op         string
    URL        string
    StatusCode int
    Err        error   // json.Unmarshal error
    Body       string  // first 512 bytes of raw body
}
```

**Rationale:**
- Callers can use `errors.As` to branch on error type: `*NetworkError` → invalidate cache + retry candidate; `*StatusError` → report HTTP code to user; `*DecodeError` → log body for debugging
- Each type carries the operation name and URL for structured logging
- `StatusError.Body` is truncated to 512 bytes to prevent log pollution from large error pages
- All types implement `Unwrap()` so `errors.Is(err, context.DeadlineExceeded)` works through the chain

**Alternative considered:** Single error type with a `Kind` field
- Rejected — Go's `errors.As` type-switch pattern works better with distinct types; a `Kind` enum requires manual switch statements and provides no compile-time exhaustiveness

**Alternative considered:** Return `(result, statusCode, error)` tuples
- Rejected — makes every call site handle three values; typed errors encode the status code for callers that need it while keeping the common `(result, error)` signature clean

### Decision 4: HTTP (not HTTPS) for intra-cluster agent communication

The `AgentClient` constructs URLs with `http://` scheme.

**Rationale:**
- The MCP server and sandbox agents communicate over the Kubernetes pod network within the same cluster and namespace
- NetworkPolicy restricts ingress to sandbox pods: only pods with `app: cli-mcp-server` label can reach port 8090
- The bearer token (HMAC-SHA256 derived) authenticates the caller — it guards against unauthorized requests from within the cluster network, not against network-level eavesdropping
- TLS between pods in the same cluster adds certificate management complexity (cert-manager, Secret rotation, mTLS handshake latency) for marginal benefit — the threat model assumes the intra-cluster network is trusted
- If cross-cluster or external-network communication is needed in future, upgrading to TLS/mTLS is a localized change in `AgentClient` (change URL scheme + configure TLS transport)

This is consistent with how the session manager and warm pool already communicate with agents (plain HTTP) and matches the parent design doc's architecture diagram.

### Decision 5: `Execute` accepts token as a parameter, not a stored field

```go
func (c *AgentClient) Execute(ctx context.Context, podIP, token string, req ExecRequest) (*ExecResponse, error)
```

The bearer token is passed per-call rather than stored in the `AgentClient`.

**Rationale:**
- Tokens are per-session (HMAC-SHA256 of session ID). A single `AgentClient` instance is shared across all sessions — storing a token would require either one client per session (wasteful) or a token-lookup function (over-engineered).
- The session manager already computes the token from `computeToken(hmacKey, sessionID)` — passing it through is natural.
- `Assign` does not use a bearer token (the `/assign` endpoint is unauthenticated, callable once). Storing a token on the client would be misleading for `Assign` calls.

### Decision 6: `HealthCheck` is a simple GET with no auth

```go
func (c *AgentClient) HealthCheck(ctx context.Context, podIP string) error
```

**Rationale:**
- The agent's `GET /health` is unauthenticated — it's used by Kubernetes readiness probes which can't inject bearer tokens
- Returns `nil` for 200, `*StatusError` for non-200, `*NetworkError` for transport failure
- Used by the session manager to verify pod readiness after warm pool claim, and potentially by the MCP server's own health check

### Decision 7: Request body and response body size safety

- Request bodies (`ExecRequest`, `AssignRequest`) are small by design — the largest field is `ExecRequest.Command` which is bounded by the bash tool handler's input validation
- Response bodies are read into memory for JSON decoding. `ExecResponse` can contain large `Stdout`/`Stderr` fields (command output). The `AgentClient` does **not** impose a size limit on successful responses — the agent itself bounds output via the bash session's timeout mechanism. If output size limiting is needed, it should be added at the agent level or the tool handler level, not in the HTTP client.
- Error response bodies (for `StatusError` and `DecodeError`) are truncated to 512 bytes to prevent log pollution

### Decision 8: No automatic retries

The `AgentClient` does not retry failed requests. Each method makes exactly one HTTP call.

**Rationale:**
- Retry semantics depend on context: `Execute` is not idempotent (running a command twice has side effects), `Assign` returns 409 on replay (safe to retry but pointless), `HealthCheck` is idempotent (safe to retry)
- The session manager already has its own retry/fallback logic (cache invalidation → rediscovery → on-demand creation)
- Adding retries in the client would conflict with context deadlines and make timeout behavior harder to reason about
- Callers are best positioned to decide retry strategy based on the typed error they receive

## Risks / Trade-offs

**Risk:** `AgentClient` and inline HTTP code coexist temporarily until `manager.go` and `pool.go` are refactored
→ **Mitigation:** The refactoring is a mechanical change — replace inline HTTP blocks with `AgentClient` method calls. It can be done in SANDBOX-1813 or as a standalone cleanup. The typed errors make the refactoring straightforward since the existing `isTransportError` logic maps directly to `errors.As(err, &NetworkError{})`.

**Risk:** `StatusError.Body` truncation at 512 bytes may lose diagnostic information for large error responses
→ **Mitigation:** 512 bytes is sufficient for typical error messages ("unauthorized", "already assigned", JSON error objects). If a specific endpoint returns larger error payloads, the limit can be increased per-endpoint later.

**Trade-off:** No response body size limit on successful `Execute` responses
→ **Accepted:** Command output size is bounded by execution timeout at the agent level. Adding a client-side limit would create a confusing failure mode where a command succeeds at the agent but fails at the client. If output is too large for the LLM context window, that's the tool handler's concern (SANDBOX-1813), not the HTTP client's.

**Trade-off:** Single shared `*http.Client` across all methods and sessions
→ **Accepted:** `http.Client` is documented as safe for concurrent use and internally manages a connection pool. Per-session clients would waste connections and prevent connection reuse between calls to the same agent pod.

## Open Questions

None — all design decisions are resolved based on the existing patterns in `manager.go`, `pool.go`, and the parent design doc.
