## Context

The CLI MCP Server architecture has two binaries: the MCP server (control plane) and the sandbox agent (data plane). SANDBOX-1806/1807 delivered the agent side — persistent bash session, HTTP handlers, and entry point. SANDBOX-1810 adds the server-side session management layer — the bridge between incoming MCP tool calls (identified by `X-Session-ID`) and the agent pods that execute commands.

The MCP server is stateless — any replica can serve any request. All durable state lives in Kubernetes labels on the sandbox pods. The session manager provides a thin caching layer (30s TTL) to avoid per-request K8s API calls while remaining correct across replicas (label-based discovery is the source of truth; cache is an optimization).

## Goals / Non-Goals

**Goals:**
- PodCache with thread-safe map and TTL-based expiry (~30s)
- SessionManager with `GetOrCreatePod` (label lookup, cache, idempotent create-or-get for pod + auth Secret using K8s API conflict handling, wait for ready)
- SessionManager with `ExecuteCommand` (resolve pod, HMAC-SHA256 bearer token, POST to agent, update last-activity annotation)
- SessionManager with `CleanupSession` (delete pod + Secret)
- SessionManager with `CleanupStale` (periodic, idle-timeout based)
- Pod spec includes: security context (RunAsNonRoot, drop ALL), readiness probe on :8090/health, kubeconfig volume, workspace emptyDir, correct labels and annotations
- SandboxConfig struct for all tunables (image, CPU/mem, idle timeout, HMAC key, warm pool size, namespace, service account, kubeconfig secret name)

**Non-Goals:**
- Warm pod pool (SANDBOX-1811)
- Agent HTTP client abstraction (SANDBOX-1812 — the manager uses `net/http` directly for now)
- MCP tool registration (SANDBOX-1813)
- MCP server entry point (SANDBOX-1814)
- Server unit tests beyond those in this PR (SANDBOX-1816)
- Metrics or structured logging middleware
- Retry logic for pod creation failures (single attempt; can be added later)

## Decisions

### Decision 1: In-memory PodCache with short TTL (30s)

Use a simple `sync.RWMutex`-protected map with 30-second TTL entries.

**Rationale:**
- The stateless MCP server can't rely on per-session state across requests — but a short-lived cache avoids a K8s API call on every single command execution within a burst
- 30s TTL means stale entries self-correct quickly if a pod gets a new IP (crash/restart)
- Conditional invalidation (`Invalidate(sessionID, podIP)`) ensures we don't accidentally evict a freshly-updated entry after a reconnect
- No external dependencies (Redis, memcached) — simplicity first

**Alternative considered:** No cache — always query K8s API
- Rejected — K8s API calls add latency (~50-100ms) to every command execution; with TTL cache, only the first request per 30s window hits the API

**Alternative considered:** Longer TTL (5 minutes)
- Rejected — if a pod restarts and gets a new IP, requests would fail for up to 5 minutes before cache expiry forces rediscovery

### Decision 2: Idempotent `GetOrCreatePod` using K8s API conflict handling

Use deterministic resource names (derived from session ID) and handle `AlreadyExists` errors from the Kubernetes API to make pod + Secret creation idempotent across replicas.

**Rationale:**
- With a stateless multi-replica server, two requests for the same `X-Session-ID` can both miss the cache and labels, then race into the create path
- By using deterministic names (`cli-mcp-sandbox-<sessionID>`, `cli-mcp-sandbox-auth-<sessionID>`) and treating `AlreadyExists` as success (fall through to discovery), only one set of resources is created
- The Kubernetes API's built-in atomicity on create operations provides the serialization point — no external locking needed
- The losing replica simply re-discovers the pod that the winning replica created

**Alternative considered:** Distributed lock (e.g., K8s Lease) before creation
- Rejected — adds complexity, latency, and a failure mode (lock not released) for a race that rarely occurs and is harmless when handled via conflict detection

### Decision 3: HMAC-SHA256 deterministic token derivation

Compute per-session auth tokens as `hex(HMAC-SHA256(HMACKey, sessionID))`.

**Rationale:**
- Any MCP server replica can derive the same token for a given session without storing or looking up tokens
- Deterministic — same key + session ID always yields same token, enabling stateless operation
- Standard cryptographic construction; 256-bit output provides ample security margin
- The token is delivered to the sandbox agent via a K8s Secret (env var injection), so the agent never computes it — it just stores and compares

**Alternative considered:** Random token stored in Secret, looked up by MCP server on each request
- Rejected — requires a K8s API call to read the Secret on each command execution, defeating the purpose of caching

### Decision 4: Label-based pod discovery as source of truth

Use label selectors (`tarsy.redhat.com/session-id=<id>, tarsy.redhat.com/component=cli-mcp-sandbox`) for pod lookup.

**Rationale:**
- Labels are indexed in etcd — list-by-label is efficient
- Any replica can discover any session's pod without shared state
- If cache misses (TTL expired, replica restart, first request), the K8s API call provides the correct answer
- Matches the parent design doc's architecture

**Alternative considered:** In-memory session-to-pod mapping as primary store
- Rejected — breaks stateless multi-replica operation; a different replica wouldn't know about a pod another replica created

### Decision 5: Create auth Secret before pod

Create the per-session Secret first, then create the pod that references it via `secretKeyRef`.

**Rationale:**
- The pod spec references the Secret via env var injection (`SANDBOX_AUTH_TOKEN` from `secretKeyRef`). If the pod starts before the Secret exists, the container will fail to start with `CreateContainerConfigError`.
- Secret-first ordering ensures the pod's env var resolution succeeds on first attempt
- If pod creation fails, we best-effort delete the orphaned Secret

**Alternative considered:** Create pod first, then Secret
- Rejected — race condition where the container starts before the Secret exists

### Decision 6: Poll-based `waitForReady` with 60s deadline

Poll the pod status every 2 seconds for up to 60 seconds until Running + PodReady condition.

**Rationale:**
- Pod startup time is typically 3-8 seconds (image pull is fast with pre-cached layers on nodes)
- 60s deadline accommodates cold image pulls or scheduling delays
- 2s poll interval is a good balance between latency and K8s API load
- Simple to implement and reason about; watch-based would add complexity for marginal latency improvement

**Alternative considered:** Watch-based with informer
- Rejected — adds significant complexity; session creation is not a hot path (happens once per investigation), so poll latency is acceptable

### Decision 7: Background annotation update after ExecuteCommand

Update `last-activity` annotation in a background goroutine (`go m.updateLastActivity()`).

**Rationale:**
- The annotation update is best-effort — it's used for stale cleanup but not for correctness
- Executing the patch synchronously would add ~50ms latency to every command response
- If the patch fails (e.g., pod deleted between exec and patch), it's logged and harmless

**Alternative considered:** Synchronous annotation update
- Rejected — adds latency to the critical path for non-critical metadata

### Decision 8: SessionManager directly uses `net/http` for agent communication

The manager constructs HTTP requests inline rather than depending on a separate agent client package.

**Rationale:**
- SANDBOX-1812 will introduce `pkg/agent/client.go` as a proper abstraction; the manager will be refactored to use it
- For now, inline HTTP keeps the session manager self-contained with zero internal dependencies beyond `pkg/agent` types
- The HTTP call is simple: POST JSON body, bearer header, decode JSON response

**Alternative considered:** Block on SANDBOX-1812 first
- Rejected — creates unnecessary sequencing; inline HTTP can be replaced later with a one-line refactor

### Decision 9: `CleanupStale` is caller-driven (no internal ticker)

The `CleanupStale` method is called externally (e.g., by a goroutine in `cmd/server/main.go` on a ticker). It does not manage its own lifecycle.

**Rationale:**
- Keeps the SessionManager testable — tests call `CleanupStale()` directly without dealing with timers
- The caller (server entry point) owns the ticker and can cancel it on shutdown
- Consistent with the design doc's "stale pod cleanup goroutine" in `runServer`

**Alternative considered:** Internal background goroutine with ticker
- Rejected — harder to test, harder to shut down cleanly, mixes lifecycle management with business logic

## Risks / Trade-offs

**Risk:** 30s cache TTL means requests can be routed to a dead pod IP for up to 30s after pod restart
→ **Mitigation:** `Invalidate()` is called on transport-level failures (connection refused, timeout, network unreachable), immediately clearing the stale entry and forcing rediscovery on the next request. Application-level HTTP errors (4xx/5xx from a reachable agent) do not invalidate the cache, since the pod is healthy.

**Risk:** `waitForReady` polls every 2s — adds 0-2s latency to pod creation
→ **Mitigation:** Acceptable for session creation (happens once per investigation). The warm pool feature (SANDBOX-1811) eliminates this latency entirely for pre-warmed pods.

**Risk:** Background `updateLastActivity` can silently fail
→ **Mitigation:** Logged at WARN level. Worst case: a pod gets cleaned up slightly before its true idle timeout. The pod would be recreated on the next request.

**Trade-off:** Manager uses inline `net/http` instead of `pkg/agent/client`
→ **Accepted:** Will be refactored in SANDBOX-1812. Keeps this story self-contained.

**Trade-off:** No retry on pod creation failure
→ **Accepted:** Single attempt returns error to caller. The MCP tool handler will propagate the error to TARSy, which can retry the tool call.

## Open Questions

None — all design decisions are resolved.
