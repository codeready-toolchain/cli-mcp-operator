## Context

The CLI MCP Server's session manager (SANDBOX-1810) creates sandbox pods on demand. Pod creation involves: create auth Secret → create Pod → poll for readiness (2s intervals, up to 60s). With cached images this takes 3–8 seconds, but cold pulls can reach 60 seconds. For TARSy investigations, this cold-start latency delays the first command.

The warm pool feature, guarded by `--warm-pool-size > 0`, pre-creates unassigned pods that sit ready and waiting. When a new session arrives, `GetOrCreatePod` claims a warm pod instead of creating one from scratch — reducing session start latency to the time needed for a label patch + `POST /assign` (typically <500ms).

## Goals / Non-Goals

**Goals:**
- `WarmPool` struct managing a pool of pre-created, unassigned sandbox pods
- `ReconcilePool()` goroutine (every 30s) that maintains the target number of unassigned pods. `WarmPoolSize` is a **cluster-wide target**, not per-replica. Every replica runs reconciliation, but each run lists the actual unassigned pod count from the K8s API and only creates the deficit. Concurrent reconcilers may temporarily overshoot (two replicas both see N < target and each create pods), but the next cycle corrects this since it observes the true cluster-wide count. This is acceptable — slight temporary overshoot is better than adding leader-election complexity for a non-critical pool sizing concern.
- Atomic claim flow: list unassigned → label patch with `resourceVersion` (first-writer-wins) → create auth Secret → `POST /assign` to agent → async replenishment. Any failure after the label patch (Secret creation failure, `/assign` timeout or connection loss) triggers full rollback: remove session-id label, delete auth Secret if created.
- Graceful fallback to on-demand creation if pool is exhausted or all claims fail
- Concurrent claims from multiple replicas do not double-assign (resourceVersion conflict detection)
- Feature is dormant when `--warm-pool-size` is 0

**Non-Goals:**
- Dynamic pool sizing based on request rate (future optimization)
- Pre-warming across multiple namespaces
- Warm pool metrics/observability (SANDBOX-1820)
- Agent HTTP client abstraction (SANDBOX-1812 — pool uses `net/http` directly, same as session manager)

## Decisions

### Decision 1: Separate `WarmPool` struct rather than adding pool methods to `SessionManager`

Create a dedicated `WarmPool` struct in `pkg/session/pool.go` that the `SessionManager` holds as an optional field.

**Rationale:**
- Keeps `SessionManager` focused on session lifecycle; pool management is a distinct concern
- `WarmPool` can be nil when pool is disabled, making the feature toggle explicit
- Easier to test independently — pool tests don't need the full session manager setup
- Follows the same pattern as `PodCache` — a focused struct owned by the manager

**Alternative considered:** Add `ReconcilePool` and `ClaimPod` directly to `SessionManager`
- Rejected — `SessionManager` already has 5 public methods; adding pool logic would make it harder to reason about

### Decision 2: Optimistic locking via resourceVersion for claim atomicity

Use Kubernetes strategic merge patch with `resourceVersion` to atomically claim a warm pod. The patch adds the `tarsy.redhat.com/session-id` label. If the resourceVersion has changed (another replica claimed it first), the patch returns a 409 Conflict.

**Rationale:**
- Kubernetes provides built-in optimistic concurrency control — no external locking needed
- The claiming replica must read the pod (to get resourceVersion), then patch. If the resourceVersion is stale, K8s rejects the patch — the replica retries with the next unassigned pod
- This is the standard Kubernetes pattern for first-writer-wins resource claiming
- The list+patch sequence handles the race between multiple replicas listing the same unassigned pod

**Alternative considered:** Label patch without resourceVersion
- Rejected — two replicas could simultaneously patch the same pod, and both would succeed. The second replica's `POST /assign` would get 409 from the agent, but the label would already be set. This creates a state where the pod is "assigned" to one session but has a different session's label. resourceVersion prevents this entirely.

**Alternative considered:** Distributed lock (Lease-based)
- Rejected — adds complexity, latency, and a failure mode (lock not released). Optimistic locking is simpler and sufficient for the expected claim rate.

### Decision 3: Warm pods created without auth Secret or session-id label

Warm pods are created with the `tarsy.redhat.com/component=cli-mcp-sandbox` label but without the `tarsy.redhat.com/session-id` label and without a `SANDBOX_AUTH_TOKEN` env var. The agent starts in "unassigned" mode and waits for `POST /assign`.

**Rationale:**
- The sandbox agent already supports an unassigned→assigned state machine (SANDBOX-1807). Warm pods start without a token. When claimed, the `POST /assign` call delivers the HMAC token.
- No session-id label means `CleanupStale` skips these pods (it only processes pods with a session-id label). Instead, warm pods with no session-id have their own staleness handling in `ReconcilePool` (see Decision 6).
- No auth Secret at creation time avoids creating orphaned Secrets for pods that may never be claimed

### Decision 4: Async replenishment after claim via buffered channel

After successfully claiming a warm pod, trigger `ReconcilePool` asynchronously via a buffered channel (size 1). The reconciler goroutine drains this channel alongside its periodic ticker.

**Rationale:**
- Claiming a pod reduces the pool by 1 — replenishing immediately keeps the pool at target size
- Non-blocking channel send means the claim path doesn't wait for the new pod to be created
- Buffered channel (size 1) coalesces multiple rapid claims into a single reconciliation
- The reconciler handles both periodic (every 30s) and event-driven (post-claim) reconciliation

**Alternative considered:** Synchronous replenishment in claim path
- Rejected — adds pod creation latency (~3-8s) to the claim path, defeating the purpose of the warm pool

### Decision 5: ClaimPod iterates unassigned pods and retries on conflict

When claiming, `ClaimPod` lists all unassigned pods, then iterates through them attempting to claim each one. If a resourceVersion conflict occurs (another replica claimed it), it moves to the next pod.

**Rationale:**
- With N replicas claiming simultaneously, only one will win each pod. Iteration with retry ensures that even under contention, a replica will eventually claim a pod (as long as the pool isn't exhausted)
- Pod ordering is by creation timestamp (oldest first) for fairness and predictability
- If all pods have been claimed (iteration exhausted), returns an error so the caller can fall back to on-demand creation

### Decision 6: Stale unassigned pods are cleaned up by the reconciler

Unassigned warm pods that have been sitting idle beyond a configurable threshold (2× IdleTimeout) are deleted by `ReconcilePool` during its periodic runs. This prevents zombie warm pods from accumulating if the pool size is reduced or the server is scaled down.

**Rationale:**
- `CleanupStale` only handles assigned pods (those with session-id labels). Unassigned pods need their own cleanup mechanism.
- Using 2× IdleTimeout gives warm pods a generous lifetime while still preventing indefinite accumulation
- The reconciler already lists unassigned pods, so adding an age check is zero additional K8s API cost

### Decision 7: Every replica runs reconciliation (no leader election)

All replicas run `ReconcilePool()` independently. Each run queries the K8s API for the actual cluster-wide count of unassigned pods and creates only the deficit to reach `WarmPoolSize`.

**Rationale:**
- `WarmPoolSize` is a cluster-wide target. The reconciler is idempotent — it measures actual state (list unassigned pods) and converges toward the target. Two replicas running concurrently may both see the same deficit and both create pods, temporarily overshooting. The next reconciliation cycle (30s later) sees the correct count and takes no action.
- Temporary overshoot (e.g., 5 pods instead of 3) is harmless — extra pods sit idle and are cleaned up after 2× IdleTimeout. Under-provisioning (missed pods due to leader failure) is worse for latency.
- Leader election adds significant complexity (K8s Lease, failover logic, loss-of-leadership handling) for marginal benefit in a system where slight overshoot is acceptable.
- The claim path already uses `resourceVersion` for atomicity, so concurrent reconcilers creating "extra" pods just means more pods available for claiming — no correctness issue.

**Alternative considered:** Leader election via K8s Lease
- Rejected — adds a failure mode (leader pod dies, lease not released, pool not replenished until new leader elected). The simplicity of every-replica reconciliation with idempotent convergence outweighs the cost of occasional temporary overshoot.

## Risks / Trade-offs

**Risk:** Pool pods consume cluster resources even when not in use
→ **Mitigation:** Pool size is 0 by default (disabled). Operators choose the size based on expected demand. Pods use the same resource requests/limits as on-demand pods.

**Risk:** Multiple replicas running `ReconcilePool` concurrently may temporarily overshoot `WarmPoolSize`
→ **Mitigation:** Each reconciliation is idempotent — it observes actual cluster state and converges. Overshoot pods sit idle and are cleaned up after 2× IdleTimeout. The next reconciliation cycle (30s) sees the correct count and stops creating. At expected replica counts (2-3), overshoot is bounded to ~2× `WarmPoolSize` in the worst case and self-corrects within one cycle.

**Risk:** Rapid bursts can exhaust the pool before replenishment
→ **Mitigation:** Fallback to on-demand creation is always available. The reconciler runs every 30s and also triggers on claim events, so the pool refills quickly.

**Risk:** resourceVersion patch can fail if the pod is modified between list and patch (e.g., K8s updates a status field)
→ **Mitigation:** The retry loop tries the next unassigned pod. In practice, K8s status updates don't change resourceVersion for metadata-only patches (strategic merge patch on labels targets metadata).

**Trade-off:** Warm pods start without auth token — there's a window between pod creation and assignment where the agent is in "unassigned" mode
→ **Accepted:** The agent rejects `/exec` calls in unassigned mode (returns 503). The `POST /assign` endpoint can only be called once (409 on replay). NetworkPolicy restricts who can reach the agent. This is the same security posture as the design doc specifies.

**Trade-off:** Pool uses `net/http` directly for `POST /assign` instead of a dedicated agent client
→ **Accepted:** Will be refactored when SANDBOX-1812 introduces `pkg/agent/client.go`. Consistent with the session manager's current approach.

## Open Questions

None — all design decisions are resolved based on the parent design doc and SANDBOX-1810 implementation patterns.
