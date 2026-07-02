## Why

The session manager creates sandbox pods on demand — each `GetOrCreatePod` call for a new session must create a Secret, create a Pod, and poll for readiness (3–8 seconds for cached images, up to 60 seconds for cold image pulls). For the first command in an investigation, this latency is directly visible to TARSy agents.

A warm pod pool pre-creates a configurable number of unassigned sandbox pods so that when a new session arrives, it can claim an already-running pod instead of waiting for one to spin up. This eliminates the cold-start penalty for the common case and degrades gracefully to on-demand creation when the pool is exhausted.

SANDBOX-1810 delivered the session manager with `GetOrCreatePod`, `ExecuteCommand`, `CleanupSession`, and `CleanupStale`. The warm pool extends this foundation by inserting a claim-from-pool step between label-based discovery and on-demand creation.

## What Changes

- Add `pkg/session/pool.go` with:
  - `WarmPool` struct holding clientset, config, HTTP client, logger, and a replenish channel
  - `ReconcilePool(ctx)` — lists unassigned pods, creates new ones to maintain target pool size
  - `ClaimPod(ctx, sessionID)` — lists unassigned pods, claims one via label patch using `resourceVersion` for first-writer-wins concurrency, creates per-session auth Secret, calls `POST /assign` on the agent to deliver the HMAC token, triggers async replenishment
  - `TriggerReplenish()` — non-blocking send to replenish channel for async pool top-up after a claim
  - `StartReconciler(ctx)` — background goroutine running `ReconcilePool` every 30 seconds and on replenish signals
  - `buildWarmPodSpec()` — similar to session manager's `buildPodSpec` but without session-id label, without auth Secret reference (agent starts in unassigned mode)

- Modify `pkg/session/manager.go`:
  - Add `pool *WarmPool` field to `SessionManager`
  - Update `NewSessionManager` to initialize a `WarmPool` when `config.WarmPoolSize > 0`
  - Update `GetOrCreatePod` to attempt `pool.ClaimPod` between label discovery and on-demand creation

- Modify `pkg/session/config.go`:
  - Add `ReconcileInterval` field (default 30s) to allow tuning the pool reconciliation frequency

## Capabilities

### New Capabilities
- `warm-pool`: Defines the warm pod pool that pre-creates unassigned sandbox pods and provides atomic claiming via Kubernetes resourceVersion-based optimistic locking

### Modified Capabilities
- `session-manager`: Updated to integrate warm pool claiming into the `GetOrCreatePod` flow between label discovery and on-demand creation

## Impact

- **Affected code**:
  - `pkg/session/pool.go` — New file with WarmPool
  - `pkg/session/pool_test.go` — New file with unit tests
  - `pkg/session/manager.go` — Modified to integrate pool claiming
  - `pkg/session/config.go` — Add ReconcileInterval field
- **API changes**: No new external HTTP endpoints. Internal Go API adds `WarmPool` type and modifies `GetOrCreatePod` behavior.
- **Dependencies**: Depends on SANDBOX-1810 (session manager, pod cache). The warm pool reuses the session manager's `buildAuthSecret`, `computeToken`, and `waitForReady` logic.
- **No new Go dependencies** — uses existing `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`.
- **No breaking changes** — when `WarmPoolSize` is 0 (default), behavior is identical to before.
