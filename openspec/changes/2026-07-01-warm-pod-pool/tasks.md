## 1. Config Update

- [ ] 1.1 Add `ReconcileInterval time.Duration` field to `SandboxConfig` in `pkg/session/config.go`
- [ ] 1.2 Set `ReconcileInterval` to 30s in `DefaultConfig()`

## 2. WarmPool — Core Structure

- [ ] 2.1 Create `pkg/session/pool.go` with `WarmPool` struct (clientset, config, httpClient, logger, replenishCh)
- [ ] 2.2 Implement `NewWarmPool(clientset, config, logger)` — initialize with buffered replenish channel (size 1) and HTTP client

## 3. WarmPool — Pod Spec

- [ ] 3.1 Implement `buildWarmPodSpec()` — construct Pod manifest with:
  - `GenerateName: "cli-mcp-sandbox-"` (not deterministic — warm pods are interchangeable)
  - Label `tarsy.redhat.com/component=cli-mcp-sandbox` (no session-id label)
  - Annotation `tarsy.redhat.com/created-at` with current UTC RFC3339 timestamp
  - Same security context, readiness probe, volumes, and resources as session manager's `buildPodSpec`
  - No `SANDBOX_AUTH_TOKEN` env var (agent starts in unassigned mode)

## 4. WarmPool — ReconcilePool

- [ ] 4.1 Implement `ReconcilePool(ctx)` — list unassigned pods (component label, no session-id label)
- [ ] 4.2 Delete stale unassigned pods (created-at > 2× IdleTimeout)
- [ ] 4.3 Count remaining non-terminal unassigned pods
- [ ] 4.4 Create new pods to reach `WarmPoolSize` target

## 5. WarmPool — ClaimPod

- [ ] 5.1 Implement `ClaimPod(ctx, sessionID)` — list unassigned pods, sort by creation timestamp (oldest first)
- [ ] 5.2 For each unassigned pod: attempt strategic merge patch to add `tarsy.redhat.com/session-id` label using `resourceVersion`
- [ ] 5.3 On conflict (409), skip to next pod; on success, proceed with assignment
- [ ] 5.4 Create auth Secret `cli-mcp-sandbox-auth-<sessionID>` with HMAC-derived token
- [ ] 5.5 Call `POST /assign` on the agent with the HMAC token in request body
- [ ] 5.6 On assign failure, rollback: remove session-id label from pod, delete auth Secret
- [ ] 5.7 On success, trigger async replenishment via `TriggerReplenish()`
- [ ] 5.8 Return claimed pod IP and name

## 6. WarmPool — Background Reconciler

- [ ] 6.1 Implement `TriggerReplenish()` — non-blocking send to buffered channel
- [ ] 6.2 Implement `StartReconciler(ctx)` — select on ticker (ReconcileInterval) and replenish channel, call `ReconcilePool` on either signal, exit on context cancellation

## 7. SessionManager Integration

- [ ] 7.1 Add `pool *WarmPool` field to `SessionManager`
- [ ] 7.2 In `NewSessionManager`, create `WarmPool` if `config.WarmPoolSize > 0`
- [ ] 7.3 Add `StartPool(ctx)` method to SessionManager that starts the pool's reconciler (if pool is non-nil)
- [ ] 7.4 Update `GetOrCreatePod`: after label discovery miss and before on-demand create, attempt `pool.ClaimPod` if pool is non-nil
- [ ] 7.5 On pool claim success: cache result, return pod IP
- [ ] 7.6 On pool claim failure: log warning, fall through to on-demand creation

## 8. Unit Tests

- [ ] 8.1 Pool tests: `buildWarmPodSpec` validation (no session-id label, no auth env, correct structure)
- [ ] 8.2 Pool tests: `ReconcilePool` creates pods to reach target size
- [ ] 8.3 Pool tests: `ReconcilePool` no-op when pool is full
- [ ] 8.4 Pool tests: `ReconcilePool` cleans up stale unassigned pods
- [ ] 8.5 Pool tests: `ClaimPod` success (label patch, Secret creation, /assign call)
- [ ] 8.6 Pool tests: `ClaimPod` handles resourceVersion conflict (retries next pod)
- [ ] 8.7 Pool tests: `ClaimPod` rolls back on assign failure
- [ ] 8.8 Pool tests: `ClaimPod` returns error when pool exhausted
- [ ] 8.9 Manager integration tests: `GetOrCreatePod` claims from pool when available
- [ ] 8.10 Manager integration tests: `GetOrCreatePod` falls back to on-demand when pool exhausted
- [ ] 8.11 Manager integration tests: `GetOrCreatePod` unchanged when pool disabled
