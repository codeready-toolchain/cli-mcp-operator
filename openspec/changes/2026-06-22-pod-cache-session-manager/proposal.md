## Why

The MCP server needs to manage sandbox pod lifecycle — creating pods on demand, caching their IPs for fast routing, executing commands via the agent HTTP API, and cleaning up idle sessions. Without a session manager, the server has no way to map an incoming `X-Session-ID` header to a running sandbox pod. Without a cache, every request would require a Kubernetes API call for pod discovery. Without cleanup, abandoned pods would accumulate indefinitely.

SANDBOX-1806/1807 delivered the sandbox agent (persistent bash + HTTP handlers). This story builds the MCP server-side counterpart that manages the agent pods from the outside.

## What Changes

- Add `pkg/session/config.go` with:
  - `SandboxConfig` struct holding all tunables (image, CPU/mem requests/limits, idle timeout, HMAC key, warm pool size, namespace, service account, kubeconfig secret name)
  - `DefaultConfig()` factory with sensible production defaults

- Add `pkg/session/cache.go` with:
  - `PodCache` struct — thread-safe `sync.RWMutex`-protected map of session ID → (pod IP, pod name, expiry)
  - TTL-based expiry (~30s default) — expired entries return cache miss
  - `Get`, `Set`, `Delete`, `Invalidate` (conditional on matching pod IP), `EvictExpired`

- Add `pkg/session/manager.go` with:
  - `SessionManager` struct holding Kubernetes clientset, config, cache, HTTP client, logger
  - `GetOrCreatePod(ctx, sessionID)` — cache → label lookup → create pod + auth Secret → wait for ready
  - `ExecuteCommand(ctx, sessionID, command, timeout)` — resolve pod, HMAC-SHA256 bearer token, POST to agent, update last-activity annotation
  - `CleanupSession(ctx, sessionID)` — delete pod + Secret, clear cache
  - `CleanupStale(ctx)` — iterate sandbox pods, delete those idle longer than IdleTimeout
  - `buildPodSpec(sessionID)` — full pod manifest with security context, readiness probe, volumes, labels, annotations
  - `buildAuthSecret(sessionID, token)` — per-session Secret for HMAC token delivery
  - `computeToken(sessionID)` — HMAC-SHA256(HMACKey, sessionID) deterministic token derivation

## Capabilities

### New Capabilities
- `pod-cache`: Defines the in-memory TTL cache for pod IP lookups with thread-safe access and conditional invalidation
- `session-manager`: Defines the session manager responsible for pod lifecycle (create, discover, proxy, cleanup) using Kubernetes labels as the source of truth

### Modified Capabilities
<!-- None — this is a new package building on existing pkg/agent types -->

## Impact

- **Affected code**:
  - `pkg/session/config.go` — New file with SandboxConfig
  - `pkg/session/cache.go` — New file with PodCache
  - `pkg/session/manager.go` — New file with SessionManager
- **API changes**: New Go API in `pkg/session` package. No external HTTP endpoints added (those come in SANDBOX-1814).
- **Dependencies**: Depends on SANDBOX-1806 (`pkg/agent` types). Depended on by SANDBOX-1811 (warm pod pool), SANDBOX-1812 (agent HTTP client — provides the transport layer the manager uses), SANDBOX-1813 (bash tool handler), SANDBOX-1814 (MCP server entry point).
- **New Go dependencies**: `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`
- **No breaking changes** — entirely new package.
