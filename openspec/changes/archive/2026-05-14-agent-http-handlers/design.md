## Context

The CLI MCP Server architecture has two binaries: the MCP server (control plane) and the sandbox agent (data plane). SANDBOX-1806 delivered the shared HTTP contract types and the persistent bash session. SANDBOX-1807 adds the HTTP layer and binary entry point — the glue between the network and the bash session.

The sandbox agent exposes three HTTP endpoints on port 8090. The MCP server calls `/exec` to run commands and `/assign` to deliver auth tokens to warm-pool pods. Kubernetes kubelet calls `/health` for readiness probes.

## Goals / Non-Goals

**Goals:**
- Expose POST /exec with bearer token auth (constant-time via `hmac.Equal()`)
- Expose POST /assign for one-time token delivery to warm-pool pods
- Expose GET /health as an unauthenticated readiness probe
- Implement agent state machine: unassigned (warm pool) → assigned (has token)
- Wire cmd/agent/main.go with HTTP server, signal handling, graceful shutdown
- Keep the handler as a thin HTTP layer — no business logic beyond parsing, auth, and serialization

**Non-Goals:**
- Container image (SANDBOX-1808)
- Unit tests (SANDBOX-1809)
- MCP server-side agent HTTP client (SANDBOX-1812)
- HMAC key derivation / per-session token computation (MCP server's responsibility)
- TLS (handled by kube-rbac-proxy on the MCP server side)
- Request logging or metrics (observability is a server-side concern)
- Content-Type validation or body size limits (network isolation + pod memory limits suffice)

## Decisions

### Decision 1: Mutex-protected AgentState struct
Use a struct with `sync.Mutex` grouping the `assigned` flag and `token` string together.

**Rationale:**
- The state machine has two coupled fields (assigned ↔ token non-empty) — a mutex groups them in one critical section
- Matches internal patterns (e.g. `VMShutdownTracker` with `RWMutex` in `mcp-server-devsandbox`)
- Negligible mutex cost for a per-pod agent serving one investigation

**Alternative considered:** Atomic operations (`atomic.Bool` + `atomic.Value`)
- Rejected — requires coordinating two atomic fields for a compound invariant; lock-free performance advantage is irrelevant at per-pod scale

**Alternative considered:** `sync.Once` for assignment
- Rejected — can't distinguish "first call" from "already called" without additional state

### Decision 2: `hmac.Equal()` for token comparison
Use `crypto/hmac.Equal()` for constant-time bearer token comparison.

**Rationale:**
- Returns clean `bool` (vs `subtle.ConstantTimeCompare` returning `int`)
- Functionally identical — `hmac.Equal()` is a thin wrapper around `subtle.ConstantTimeCompare()`
- Semantically appropriate since the token is HMAC-derived by the MCP server (`HMAC-SHA256(HMACKey, sessionID)`)

**Alternative considered:** `crypto/subtle.ConstantTimeCompare()`
- Rejected — awkward `int` return; no functional difference

### Decision 3: Handler struct with method handlers
Use a `Handler` struct holding `*BashSession` and `*AgentState`, with `HandleExec`, `HandleAssign`, `HandleHealth` as methods.

**Rationale:**
- Three handlers share the same two dependencies — a struct groups them once
- Easy to test: construct with real or mock dependencies
- Standard Go HTTP handler pattern

**Alternative considered:** Closures over shared state
- Rejected — would repeat `(session, state)` parameter lists three times; closures are better when handlers have different dependencies

### Decision 4: `net/http.Server` with `Shutdown()` for graceful drain
Use `http.Server.Shutdown()` with a 310-second timeout (MaxTimeout 300s + 10s buffer).

**Rationale:**
- In-flight `/exec` requests can run up to 300s and must drain before exit
- `Shutdown()` stops accepting new connections, closes idle keepalives, waits for active requests
- 310s ensures even the longest command finishes with buffer for HTTP overhead
- Kubernetes pod spec must set `terminationGracePeriodSeconds: 310` to match

**Alternative considered:** Bare `http.ListenAndServe` + immediate exit
- Rejected — kills in-flight `/exec` requests mid-execution

### Decision 5: Go 1.22+ method-based ServeMux routing
Use `mux.HandleFunc("POST /exec", ...)` instead of manual method checks.

**Rationale:**
- Repo is Go 1.24+; `use-modern-go` skill says to use modern idioms
- Automatic 405 Method Not Allowed for wrong methods — correct HTTP semantics for free
- Less boilerplate in handlers

**Alternative considered:** Plain paths with manual method checks
- Rejected — boilerplate in every handler, pre-1.22 pattern in a 1.24+ repo

### Decision 6: Timeout clamping in BashSession only
The handler converts `int` seconds to `time.Duration` and passes it through. BashSession applies defaults and caps internally (lines 135-139 of `bash.go`).

**Rationale:**
- BashSession already clamps correctly — no duplication needed
- No change to existing SANDBOX-1806 code
- Follows "parse at boundaries, trust internals" — BashSession is the authority on its own limits

**Alternative considered:** Handler clamps too (defense in depth)
- Rejected — duplicate logic, two places to update if limits change

### Decision 7: Handler lives in `pkg/sandbox/handler.go`
Co-located with `bash.go` since the handler is the sole consumer of `BashSession`.

**Rationale:**
- Matches the parent design doc, implementation plan, and stories
- Cross-package import on `pkg/agent` is a one-way dependency on pure data types
- Handler is ~100 lines — doesn't warrant its own package

### Decision 8: POST /assign returns 200 with empty body
Simple, consistent with design docs. No response body needed.

**Alternative considered:** 204 No Content
- Rejected — minor deviation from design docs with no practical benefit

### Decision 9: No input validation (Content-Type, body size)
Agent runs in an isolated pod network (NetworkPolicy restricts ingress to MCP server only). Pod memory limits (512Mi) are the backstop.

**Rationale:**
- The MCP server is our code sending tiny well-formed payloads
- Body size limits protect against a scenario requiring a bug in our own code, in an isolated network, with existing safeguards
- Simplicity first

## Risks / Trade-offs

**Risk:** `/assign` is unauthenticated — anyone who can reach the agent can deliver a token
→ **Mitigation:** NetworkPolicy restricts ingress to pods labeled `app: cli-mcp-server` only. The warm-pool pod has no token yet, so there's nothing to authenticate with.

**Risk:** 310s shutdown timeout means pod deletion takes up to ~5 minutes worst case
→ **Mitigation:** Only occurs during active investigation cleanup or node drain. The alternative (killing in-flight commands) is worse.

**Risk:** `writeError` ignores `json.Encode` error return
→ **Mitigation:** `ErrorResponse` is a simple struct that can't fail to serialize. Match `mcp-server-devsandbox` pattern with `_ = json.NewEncoder(w).Encode(...)` to satisfy `errcheck` linter.

**Trade-off:** Handler doesn't clamp timeout — relies entirely on BashSession
→ **Accepted:** BashSession already handles this correctly. A negative or zero timeout from the request is treated as "use default" by BashSession's `<= 0` check.

## Open Questions

None — all design decisions are resolved.
