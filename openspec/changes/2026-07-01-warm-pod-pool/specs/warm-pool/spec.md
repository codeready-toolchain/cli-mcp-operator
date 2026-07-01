## ADDED Requirements

### Requirement: WarmPool maintains a target number of unassigned pods

The `WarmPool` SHALL maintain a configurable number of pre-created, unassigned sandbox pods ready for immediate claiming by new sessions.

#### Scenario: ReconcilePool creates pods to reach target size
- **WHEN** `ReconcilePool` is called
- **AND** the number of non-terminal unassigned sandbox pods is less than `WarmPoolSize`
- **THEN** it SHALL create new unassigned pods until the count reaches `WarmPoolSize`
- **AND** each new pod SHALL have label `tarsy.redhat.com/component=cli-mcp-sandbox`
- **AND** each new pod SHALL NOT have a `tarsy.redhat.com/session-id` label
- **AND** each new pod SHALL NOT have a `SANDBOX_AUTH_TOKEN` env var

#### Scenario: ReconcilePool is a no-op when pool is full
- **WHEN** `ReconcilePool` is called
- **AND** the number of non-terminal unassigned sandbox pods equals or exceeds `WarmPoolSize`
- **THEN** it SHALL NOT create any new pods

#### Scenario: ReconcilePool cleans up stale unassigned pods
- **WHEN** `ReconcilePool` is called
- **AND** an unassigned pod's `created-at` annotation indicates age exceeding 2× IdleTimeout
- **THEN** it SHALL delete the stale pod before counting remaining pods for replenishment

#### Scenario: Pool is disabled when WarmPoolSize is 0
- **WHEN** `WarmPoolSize` is 0
- **THEN** no `WarmPool` SHALL be created
- **AND** the session manager SHALL skip the claim step in `GetOrCreatePod`

### Requirement: ClaimPod atomically assigns a warm pod to a session

The `ClaimPod` method SHALL claim an unassigned warm pod for a given session using optimistic locking, create its auth Secret, and deliver the token to the agent.

#### Scenario: Successful claim from pool
- **WHEN** `ClaimPod` is called with a session ID
- **AND** at least one unassigned pod is available in the pool
- **THEN** it SHALL patch the oldest unassigned pod to add `tarsy.redhat.com/session-id=<sessionID>` using the pod's `resourceVersion` for optimistic locking
- **AND** it SHALL create an auth Secret `cli-mcp-sandbox-auth-<sessionID>` containing the HMAC-derived token
- **AND** it SHALL call `POST /assign` on the agent with the HMAC token
- **AND** it SHALL trigger async pool replenishment
- **AND** it SHALL return the claimed pod's IP and name

#### Scenario: Concurrent claim conflict is handled via retry
- **WHEN** two replicas simultaneously attempt to claim the same unassigned pod
- **AND** one replica's label patch returns a 409 Conflict (resourceVersion mismatch)
- **THEN** the losing replica SHALL skip that pod and attempt to claim the next unassigned pod
- **AND** no duplicate assignments SHALL occur

#### Scenario: Pool exhausted returns error
- **WHEN** `ClaimPod` is called
- **AND** no unassigned pods are available (all claimed or none exist)
- **THEN** it SHALL return an error indicating pool exhaustion
- **AND** the caller SHALL fall back to on-demand pod creation

#### Scenario: Assign call failure rolls back the claim
- **WHEN** a pod's label is successfully patched (claimed)
- **AND** the `POST /assign` call to the agent fails
- **THEN** the claim SHALL be rolled back by removing the session-id label from the pod
- **AND** the auth Secret SHALL be deleted
- **AND** an error SHALL be returned

### Requirement: Warm pod spec matches on-demand pod spec without session-specific fields

The `buildWarmPodSpec` method SHALL create a pod spec identical to the session manager's `buildPodSpec` except for session-specific fields.

#### Scenario: Warm pod spec has correct structure
- **WHEN** `buildWarmPodSpec` is called
- **THEN** the pod SHALL use `GenerateName: "cli-mcp-sandbox-"` (not a deterministic name)
- **AND** the pod SHALL have label `tarsy.redhat.com/component=cli-mcp-sandbox`
- **AND** the pod SHALL NOT have a `tarsy.redhat.com/session-id` label
- **AND** the pod SHALL have annotation `tarsy.redhat.com/created-at` with current UTC time in RFC3339
- **AND** the pod SHALL NOT have a `SANDBOX_AUTH_TOKEN` env var (agent starts in unassigned mode)
- **AND** all other pod spec fields (security context, probes, volumes, resources) SHALL match `buildPodSpec`

### Requirement: StartReconciler runs periodic and event-driven reconciliation

The `StartReconciler` method SHALL run a background goroutine that reconciles the pool periodically and on demand.

#### Scenario: Periodic reconciliation
- **WHEN** `StartReconciler` is called
- **THEN** it SHALL run `ReconcilePool` every `ReconcileInterval` (default 30s) using a ticker

#### Scenario: Event-driven reconciliation after claim
- **WHEN** `TriggerReplenish` is called (after a successful claim)
- **THEN** `ReconcilePool` SHALL run promptly (not waiting for the next ticker tick)

#### Scenario: Reconciler respects context cancellation
- **WHEN** the context passed to `StartReconciler` is cancelled
- **THEN** the reconciler goroutine SHALL exit

### Requirement: GetOrCreatePod integrates pool claiming

The session manager's `GetOrCreatePod` SHALL attempt to claim a warm pod before falling back to on-demand creation.

#### Scenario: Pool claim succeeds
- **WHEN** `GetOrCreatePod` is called for a new session
- **AND** cache and label discovery both miss
- **AND** the warm pool is enabled and has available pods
- **THEN** it SHALL claim a warm pod via `pool.ClaimPod`
- **AND** cache the result
- **AND** return the pod IP

#### Scenario: Pool claim fails, falls back to on-demand creation
- **WHEN** `GetOrCreatePod` is called for a new session
- **AND** the warm pool is enabled but exhausted (or claim fails)
- **THEN** it SHALL fall back to on-demand pod creation (existing behavior)
- **AND** log a warning about pool exhaustion

#### Scenario: Pool disabled, existing behavior unchanged
- **WHEN** `GetOrCreatePod` is called
- **AND** `WarmPoolSize` is 0 (pool disabled)
- **THEN** the behavior SHALL be identical to SANDBOX-1810 (cache → discover → create)

## MODIFIED Requirements

### Requirement: SandboxConfig includes ReconcileInterval

#### Scenario: DefaultConfig includes ReconcileInterval
- **WHEN** `DefaultConfig()` is called
- **THEN** `ReconcileInterval` SHALL be 30 seconds
