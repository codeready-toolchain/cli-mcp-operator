## ADDED Requirements

### Requirement: SandboxConfig holds all tunables with sensible defaults

The `SandboxConfig` struct SHALL hold all configurable parameters for sandbox pod lifecycle, and `DefaultConfig()` SHALL return production-ready defaults.

#### Scenario: DefaultConfig provides production defaults
- **WHEN** `DefaultConfig()` is called
- **THEN** CPURequest SHALL be "100m", CPULimit "500m", MemoryRequest "128Mi", MemoryLimit "512Mi"
- **AND** IdleTimeout SHALL be 30 minutes
- **AND** WarmPoolSize SHALL be 0 (disabled)
- **AND** Namespace SHALL be "tarsy"
- **AND** ServiceAccountName SHALL be "cli-mcp-investigation-sa"
- **AND** KubeconfigSecret SHALL be "cli-mcp-investigation-kubeconfig"

### Requirement: Session IDs are validated before use in Kubernetes resource names

Session IDs are interpolated into Kubernetes resource names (pod names, Secret names) and label values. They MUST be validated or normalized before use to ensure compatibility with Kubernetes naming rules.

#### Scenario: Valid session ID is accepted
- **WHEN** a session ID matches the pattern `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` (RFC 1123 DNS label)
- **THEN** it SHALL be accepted and used directly in resource names

#### Scenario: Invalid session ID is rejected
- **WHEN** a session ID contains characters outside `[a-z0-9-]`, starts/ends with a hyphen, or exceeds 63 characters
- **THEN** `GetOrCreatePod` SHALL return a validation error without making any K8s API calls

### Requirement: GetOrCreatePod resolves or creates a sandbox pod for a session

The `GetOrCreatePod` method SHALL implement a three-tier lookup: cache → label-based K8s API discovery → idempotent create-or-get new pod (handling `AlreadyExists` conflicts from concurrent replicas).

#### Scenario: Cache hit returns cached pod IP
- **WHEN** `GetOrCreatePod` is called for a session with a valid cache entry
- **THEN** it SHALL return the cached pod IP without querying the K8s API

#### Scenario: Cache miss triggers label-based discovery
- **WHEN** `GetOrCreatePod` is called for a session with no cache entry
- **AND** a Ready pod exists with matching session-id label (PodReady condition is true)
- **THEN** it SHALL return the pod's IP and cache the result
- **AND** if multiple matching Ready pods exist, it SHALL select the oldest by creation timestamp for deterministic behavior

#### Scenario: No existing pod triggers creation
- **WHEN** `GetOrCreatePod` is called for a session with no cache entry
- **AND** no pod exists with matching session-id label
- **THEN** it SHALL create an auth Secret, create a sandbox pod, wait for ready, cache the result, and return the pod IP

#### Scenario: Concurrent create is idempotent via conflict handling
- **WHEN** two replicas simultaneously attempt to create a pod for the same session ID
- **AND** one replica's K8s create call returns `AlreadyExists`
- **THEN** the losing replica SHALL fall through to label-based discovery and return the existing pod's IP
- **AND** no duplicate pods or Secrets SHALL be created

#### Scenario: Pod creation creates auth Secret first
- **WHEN** a new sandbox pod is created for a session
- **THEN** the per-session auth Secret SHALL be created before the Pod
- **AND** the Secret name SHALL be `cli-mcp-sandbox-auth-<sessionID>`
- **AND** the Secret SHALL contain the HMAC-derived token in the `token` key

#### Scenario: Pod creation waits for readiness
- **WHEN** a new sandbox pod is created
- **THEN** `GetOrCreatePod` SHALL poll the pod status every 2 seconds
- **AND** return success only when the pod is Running with PodReady condition true
- **AND** timeout after 60 seconds with an error

### Requirement: Pod spec follows security and operational standards

The pod manifest built by `buildPodSpec` SHALL include all required security context, volumes, probes, and labels from the design doc.

#### Scenario: Pod has correct labels and annotations
- **WHEN** `buildPodSpec` is called with a session ID
- **THEN** the pod SHALL have label `tarsy.redhat.com/session-id` = sessionID
- **AND** label `tarsy.redhat.com/component` = "cli-mcp-sandbox"
- **AND** annotation `tarsy.redhat.com/created-at` = current UTC time in RFC3339
- **AND** annotation `tarsy.redhat.com/last-activity` = current UTC time in RFC3339

#### Scenario: Pod has restrictive security context
- **WHEN** `buildPodSpec` is called
- **THEN** the pod-level SecurityContext SHALL set RunAsNonRoot=true, RunAsUser=1001, RunAsGroup=1001
- **AND** the container-level SecurityContext SHALL set AllowPrivilegeEscalation=false
- **AND** the container-level Capabilities SHALL Drop ALL

#### Scenario: Pod has readiness probe on agent health endpoint
- **WHEN** `buildPodSpec` is called
- **THEN** the container SHALL have an HTTP readiness probe on /health port 8090
- **AND** InitialDelaySeconds SHALL be 2
- **AND** PeriodSeconds SHALL be 10

#### Scenario: Pod has kubeconfig and workspace volumes
- **WHEN** `buildPodSpec` is called
- **THEN** the pod SHALL have a "kubeconfig" volume from the configured Secret (read-only mount at /config)
- **AND** a "workspace" emptyDir volume (writable mount at /workspace)

#### Scenario: Pod env delivers auth token via SecretKeyRef
- **WHEN** `buildPodSpec` is called for a session
- **THEN** the SANDBOX_AUTH_TOKEN env var SHALL reference the per-session Secret's "token" key
- **AND** KUBECONFIG SHALL be "/config/kubeconfig"
- **AND** HOME SHALL be "/workspace"

### Requirement: ExecuteCommand proxies commands to the sandbox agent

The `ExecuteCommand` method SHALL resolve the pod, authenticate with HMAC, POST to the agent, and return the response.

#### Scenario: Successful command execution
- **WHEN** `ExecuteCommand` is called with a valid session ID and command
- **THEN** it SHALL POST to `http://<podIP>:8090/exec` with JSON body containing command and timeout
- **AND** include `Authorization: Bearer <HMAC-token>` header
- **AND** return the decoded `ExecResponse`

**Trust boundary note:** HTTP (not HTTPS) is used because the MCP server and sandbox agent pods communicate over the Kubernetes pod network within the same cluster. The bearer token guards against unauthorized callers within the cluster network, not against network-level eavesdropping. If cross-cluster or external-network communication is needed in future, this should be upgraded to TLS/mTLS.

#### Scenario: HMAC token derivation is deterministic
- **WHEN** `computeToken` is called with the same session ID and HMAC key
- **THEN** it SHALL always return the same hex-encoded HMAC-SHA256 value
- **AND** different session IDs SHALL produce different tokens

#### Scenario: Agent unreachable invalidates cache
- **WHEN** the HTTP request to the agent fails with a transport-level error (connection refused, timeout, DNS resolution failure, or network unreachable)
- **THEN** the cache entry SHALL be invalidated for that session and pod IP
- **AND** an error SHALL be returned
- **AND** application-level HTTP errors (4xx/5xx responses from a reachable agent) SHALL NOT trigger cache invalidation

#### Scenario: Last-activity annotation is updated in background
- **WHEN** a command executes successfully
- **THEN** the pod's `tarsy.redhat.com/last-activity` annotation SHALL be patched with the current UTC timestamp
- **AND** the patch SHALL run in a background goroutine (not blocking the response)

### Requirement: CleanupSession deletes a session's pod and Secret

The `CleanupSession` method SHALL remove all Kubernetes resources for a session and clear the cache.

#### Scenario: Full session cleanup
- **WHEN** `CleanupSession` is called with a session ID
- **THEN** all pods with matching session-id label SHALL be deleted
- **AND** the auth Secret `cli-mcp-sandbox-auth-<sessionID>` SHALL be deleted
- **AND** the cache entry SHALL be removed

### Requirement: CleanupStale removes idle sessions exceeding the timeout

The `CleanupStale` method SHALL iterate all sandbox pods and clean up those whose last-activity exceeds IdleTimeout.

#### Scenario: Stale pod is cleaned up
- **WHEN** `CleanupStale` is called
- **AND** a pod's `last-activity` annotation indicates idle time exceeding IdleTimeout
- **THEN** the session SHALL be cleaned up via `CleanupSession`

#### Scenario: Fresh pod is preserved
- **WHEN** `CleanupStale` is called
- **AND** a pod's `last-activity` annotation indicates idle time within IdleTimeout
- **THEN** the pod SHALL NOT be deleted

#### Scenario: Pod without last-activity uses created-at as fallback
- **WHEN** `CleanupStale` encounters a pod with no `last-activity` annotation
- **THEN** it SHALL use the `created-at` annotation for idle time calculation
