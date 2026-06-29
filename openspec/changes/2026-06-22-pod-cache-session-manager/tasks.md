## 1. SandboxConfig

- [ ] 1.1 Create `pkg/session/config.go` with `SandboxConfig` struct (Image, CPURequest, CPULimit, MemoryRequest, MemoryLimit, IdleTimeout, HMACKey, WarmPoolSize, Namespace, ServiceAccountName, KubeconfigSecret)
- [ ] 1.2 Implement `DefaultConfig()` returning sensible defaults (100m/500m CPU, 128Mi/512Mi mem, 30m idle, tarsy namespace)

## 2. PodCache

- [ ] 2.1 Create `pkg/session/cache.go` with `PodCache` struct (`sync.RWMutex`, `map[string]*podEntry`, `ttl`)
- [ ] 2.2 Define `podEntry` struct (podIP, podName, expiresAt)
- [ ] 2.3 Implement `NewPodCache(ttl)` — default to 30s if ttl <= 0
- [ ] 2.4 Implement `Get(sessionID)` — return pod IP and name if entry exists and not expired
- [ ] 2.5 Implement `Set(sessionID, podIP, podName)` — store with fresh TTL
- [ ] 2.6 Implement `Delete(sessionID)` — unconditional removal
- [ ] 2.7 Implement `Invalidate(sessionID, podIP)` — conditional removal (only if stored IP matches)
- [ ] 2.8 Implement `EvictExpired()` — remove all expired entries, return count
- [ ] 2.9 Implement `Len()` — return entry count (including expired but not yet evicted)

## 3. SessionManager — Pod Discovery and Creation

- [ ] 3.1 Create `pkg/session/manager.go` with `SessionManager` struct (clientset, config, cache, httpClient, logger)
- [ ] 3.2 Implement `NewSessionManager(clientset, config, logger)` — initialize cache and HTTP client
- [ ] 3.3 Implement `discoverPod(ctx, sessionID)` — list pods by label selector, return oldest Ready pod's IP (PodReady condition true, deterministic by creation timestamp). If no Ready pod but a non-terminal pod exists (Pending/Running), wait for it via `waitForReady`. Skip terminal pods (Failed/Succeeded).
- [ ] 3.4 Implement `buildPodSpec(sessionID)` — construct Pod manifest with:
  - Name `cli-mcp-sandbox-<sessionID>` (deterministic, required for idempotent create-or-get via `AlreadyExists` handling)
  - Labels: `tarsy.redhat.com/session-id`, `tarsy.redhat.com/component`
  - Annotations: `tarsy.redhat.com/created-at`, `tarsy.redhat.com/last-activity`
  - SecurityContext: RunAsNonRoot, RunAsUser 1001, RunAsGroup 1001
  - Container security: AllowPrivilegeEscalation=false, Drop ALL capabilities
  - ReadinessProbe: HTTPGet /health:8090, initialDelay 2s, period 10s
  - VolumeMounts: kubeconfig (ro at /config), workspace (rw at /workspace)
  - Env: KUBECONFIG, HOME, SANDBOX_AUTH_TOKEN (from SecretKeyRef)
- [ ] 3.5 Implement `buildAuthSecret(sessionID, token)` — create Secret with token in StringData
- [ ] 3.6 Implement `computeToken(sessionID)` — HMAC-SHA256(HMACKey, sessionID), hex-encoded
- [ ] 3.7 Implement `waitForReady(ctx, podName)` — poll every 2s for up to 60s until Running + PodReady
- [ ] 3.8 Implement `createSandboxPod(ctx, sessionID)` — create Secret, create Pod, wait for ready
- [ ] 3.9 Implement `validateSessionID(sessionID)` — reject IDs not matching RFC 1123 DNS label format (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
- [ ] 3.10 Implement `GetOrCreatePod(ctx, sessionID)` — validate session ID, then cache hit → label discovery → idempotent create-or-get (handle `AlreadyExists` conflicts)

## 4. SessionManager — Command Execution

- [ ] 4.1 Implement `ExecuteCommand(ctx, sessionID, command, timeoutSec)` — resolve pod, compute token, POST to agent /exec
- [ ] 4.2 On transport-level error (connection refused, timeout, network unreachable), call `cache.Invalidate(sessionID, podIP)` before returning error — application-level HTTP errors (4xx/5xx) SHALL NOT invalidate cache
- [ ] 4.3 On success, decode `agent.ExecResponse` from JSON body
- [ ] 4.4 Launch background `updateLastActivity(sessionID)` goroutine after successful exec
- [ ] 4.5 Implement `updateLastActivity(sessionID)` — patch pod annotation with current RFC3339 timestamp

## 5. SessionManager — Cleanup

- [ ] 5.1 Implement `CleanupSession(ctx, sessionID)` — delete cache entry, list+delete pods by label, delete auth Secret
- [ ] 5.2 Implement `CleanupStale(ctx)` — list all sandbox pods, parse last-activity annotation, cleanup those exceeding IdleTimeout

## 6. Unit Tests

- [ ] 6.1 Cache tests: set/get, TTL expiry, delete, invalidate (match/mismatch), evict expired, concurrent access, overwrite
- [ ] 6.2 Manager tests: cache hit, discover existing pod, create pod spec validation, auth secret structure, token determinism
- [ ] 6.3 Manager tests: cleanup session (pod + secret deleted, cache cleared)
- [ ] 6.4 Manager tests: cleanup stale (stale pod deleted, fresh pod preserved)
- [ ] 6.5 Manager tests: execute command via httptest server (token sent, request proxied, response decoded)
