# CLI MCP Server — Jira Stories

**Epic:** [SANDBOX-1803](https://redhat.atlassian.net/browse/SANDBOX-1803)
**Design Proposal:** [mcp-common PR #4](https://github.com/codeready-toolchain/mcp-common/pull/4)
**Implementation Plan:** [cli_mcp_server_implementation.plan.md](../../../.cursor/plans/cli_mcp_server_implementation_acebd3ea.plan.md)
**Generated:** 2026-05-05

---

## Context

This epic delivers a new MCP server (`cli-mcp-server`) that gives TARSy investigation agents a sandboxed bash shell inside per-investigation Kubernetes pods. Two binaries in one repo:

- **cli-mcp-server** (control plane) — manages pod lifecycle via client-go, exposes the `bash` MCP tool, proxies commands to sandbox agents
- **sandbox-agent** (data plane) — runs inside each sandbox pod, manages a persistent bash process with oc/kubectl/jq/yq/curl

Stories are ordered by dependency. Phase 1 builds the sandbox agent, Phase 2 builds the MCP server, Phase 3 handles deployment and TARSy wiring, Phase 4 covers production hardening.

---

## Phase 1: Sandbox Agent

### ☐ SANDBOX-1805: Repo scaffold

As a developer I want the `cli-mcp-server` repository created with the standard codeready-toolchain project structure so that all subsequent stories have a consistent foundation to build on.

**Acceptance Criteria**
- Repo has `go.mod`, `Makefile`, `.gitignore`, `.govulncheck.yaml`, CI workflows — all mirroring mcp-server-devsandbox
- `pkg/version/version.go` with ldflags-injected `Commit` and `BuildTime`
- Empty directory structure for `cmd/server/`, `cmd/agent/`, `pkg/session/`, `pkg/agent/`, `pkg/sandbox/`, `pkg/server/`, `pkg/tools/`
- `go build ./...`, `make test`, and `make lint` pass

---

### ☐ SANDBOX-1806: Shared types and persistent bash session

As a developer I want the HTTP contract types and a robust persistent bash session so that the agent can execute arbitrary shell commands with reliable output capture.

**Acceptance Criteria**
- `pkg/agent/types.go` defines `ExecRequest`, `ExecResponse`, `AssignRequest` structs shared between agent and server
- `pkg/sandbox/bash.go` implements `BashSession` with UUID-based delimiter protocol, timeout handling (SIGKILL to process group, not bash), and crash recovery (respawn on EOF)
- Environment variables persist across calls
- A timed-out command is killed but the bash session survives for the next command
- If bash crashes, next Execute respawns and reports "session state was reset" in stderr

**Depends on:** SANDBOX-1805

---

### ☐ SANDBOX-1807: Sandbox agent HTTP handlers and entry point

As a developer I want the sandbox agent to expose HTTP endpoints so that the MCP server can execute commands, deliver auth tokens, and check health over the pod network.

**Acceptance Criteria**
- `POST /exec` — bearer token auth (constant-time via `hmac.Equal()`), returns `ExecResponse`; 401 without valid token
- `POST /assign` — one-time token delivery for warm-pool pods; 409 if already assigned
- `GET /health` — unauthenticated; 200 if bash alive, 503 if not
- Agent state machine: starts `unassigned` (rejects `/exec`) or `assigned` (if `SANDBOX_AUTH_TOKEN` env var set)
- `cmd/agent/main.go` starts HTTP on `:8090`, shuts down gracefully on SIGTERM

**Depends on:** SANDBOX-1806

---

### ☐ SANDBOX-1808: Agent container image

As a developer I want a container image for the sandbox agent that includes oc, kubectl, jq, yq, and curl so that investigation commands have the CLIs they need.

**Acceptance Criteria**
- `Containerfile.agent` multi-stage build on `oc-client-base-minimal`, non-root (UID 1001)
- `oc`, `kubectl`, `jq`, `yq`, `curl` available in PATH
- Container starts the agent on port 8090

**Depends on:** SANDBOX-1807

---

### ☐ SANDBOX-1809: Sandbox agent unit tests

As a developer I want comprehensive tests for the bash session and HTTP handlers so that regressions are caught before merge.

**Acceptance Criteria**
- `bash_test.go` covers: execution, exit codes, env persistence, crash recovery, timeout, delimiter edge cases
- `handler_test.go` covers: auth enforcement (missing/wrong/correct token), assign state machine, health endpoint
- `go test ./pkg/sandbox/...` passes

**Depends on:** SANDBOX-1806, SANDBOX-1807

---

## Phase 2: MCP Server Core

### ☐ SANDBOX-1810: Pod cache and session manager

As a developer I want a session manager that handles sandbox pod lifecycle so that the MCP tool handler has a clean interface for session-scoped command execution.

**Acceptance Criteria**
- `PodCache` with thread-safe map and TTL-based expiry (~30s)
- `SessionManager` with `GetOrCreatePod` (label lookup, cache, create pod + auth Secret, wait for ready), `ExecuteCommand` (resolve pod, HMAC-SHA256 bearer token, POST to agent, update `last-activity` annotation), `CleanupSession` (delete pod + Secret), `CleanupStale` (periodic, idle-timeout based)
- Pod spec includes: security context (RunAsNonRoot, drop ALL), readiness probe on `:8090/health`, kubeconfig volume, workspace emptyDir, correct labels and annotations
- Uses `SandboxConfig` struct for all tunables (image, CPU/mem, idle timeout, HMAC key, warm pool size)

**Depends on:** SANDBOX-1805

---

### ☐ SANDBOX-1811: Warm pod pool

As a developer I want a warm pod pool that pre-creates unassigned sandbox pods so that the first command in an investigation doesn't wait for pod scheduling.

**Acceptance Criteria**
- `ReconcilePool()` goroutine (every 30s) maintains target number of unassigned pods
- Claim flow: label patch (first-writer-wins via resourceVersion), create auth Secret, call `POST /assign`, async replenishment
- Falls back to on-demand creation if pool exhausted
- Concurrent claims don't double-assign (resourceVersion conflict handled)
- Only active when `--warm-pool-size > 0`

**Depends on:** SANDBOX-1810

---

### ☐ SANDBOX-1812: Agent HTTP client

As a developer I want a typed HTTP client for the sandbox agent API so that the MCP server can call `/exec`, `/assign`, and `/health` with proper error handling.

**Acceptance Criteria**
- `AgentClient` wraps `http.Client` with `Execute`, `Assign`, `HealthCheck` methods
- Configurable timeouts
- Returns typed errors for network failures, non-200 responses, and JSON decode failures

**Depends on:** SANDBOX-1806

---

### ☐ SANDBOX-1813: Bash MCP tool handler

As a developer I want the `bash` MCP tool registered so that TARSy agents can execute shell commands via the standard MCP tool-call interface.

**Acceptance Criteria**
- Extracts `X-Session-ID` from request headers; rejects with clear error if missing
- Clamps timeout: default 60s, max 300s
- Non-zero exit codes set `IsError: true` on the result but still return output (useful for the LLM)
- Only infrastructure failures (pod creation, agent unreachable) are MCP-level errors
- Returns structured `BashOutput{Stdout, Stderr, ExitCode, DurationMs}` as JSON in `mcp.TextContent`

**Depends on:** SANDBOX-1810, SANDBOX-1812

---

### ☐ SANDBOX-1814: MCP server setup and entry point

As a developer I want the MCP server wired together with middleware, HTTP endpoints, and Cobra CLI so that it can run as a production-ready service.

**Acceptance Criteria**
- `mcp.Server` with mcp-common `MetricsMiddleware` + `LoggingMiddleware`, following mcp-server-devsandbox pattern
- `StreamableHTTPHandler` with `Stateless: true`, `DisableLocalhostProtection: true`
- HTTP mux: `/mcp`, `DELETE /sessions/{id}` (returns 204), `/metrics`, `/live`, `/health` (verifies K8s API reachable)
- Cobra CLI with flags: `--address`, `--namespace`, `--sandbox-image`, `--kubeconfig`, `--hmac-key-file`, `--idle-timeout`, `--warm-pool-size`
- Graceful shutdown on SIGTERM

**Depends on:** SANDBOX-1813

---

### ☐ SANDBOX-1815: Server container image

As a developer I want a minimal distroless container image for the MCP server.

**Acceptance Criteria**
- `Containerfile.server` multi-stage build, runtime on `gcr.io/distroless/static-debian12`, non-root (UID 1001)
- Default CMD: `["--transport", "http", "--stateless"]`

**Depends on:** SANDBOX-1814

---

### ☐ SANDBOX-1816: Server unit tests

As a developer I want unit tests for all server-side packages so that regressions in pod management and tool handling are caught.

**Acceptance Criteria**
- Tests for session manager, pool, cache, agent client, bash tool, and server setup
- Uses `fake.NewSimpleClientset()` and `httptest.NewServer` — no real cluster required
- Both happy paths and error paths covered
- `go test ./pkg/...` passes

**Depends on:** SANDBOX-1810, SANDBOX-1811, SANDBOX-1812, SANDBOX-1813, SANDBOX-1814

---

### ☐ SANDBOX-1817: CI/CD pipelines

As a developer I want GitHub Actions workflows that build, test, and publish both container images on merge.

**Acceptance Criteria**
- `ci.yml` triggers on PRs: builds both binaries, runs tests and vet
- `cd.yml` triggers on master merge: builds and pushes `quay.io/codeready-toolchain/cli-mcp-server` and `quay.io/codeready-toolchain/cli-mcp-sandbox` via buildah

**Depends on:** SANDBOX-1808, SANDBOX-1815

---

## Phase 3: Deployment and TARSy Integration

### ☐ SANDBOX-1818: Kustomize manifests, RBAC, and NetworkPolicy

As a developer I want production-ready Kubernetes manifests so that the server and sandbox pods are deployed with proper RBAC and network isolation.

**Acceptance Criteria**
- Manifests in `sandbox-sre/components/cli-mcp-server/`: Deployment (kube-rbac-proxy sidecar), Service, ServiceAccounts, ClusterRoles, NetworkPolicy, ServiceMonitor, kustomization.yaml
- MCP server SA: pods (create/delete/get/list/watch/patch) + secrets (create/delete) + `system:auth-delegator`
- Investigation SA: `view` + `list-nodes` + `kube-investigation-readonly` — explicitly no `pods/exec` or KubeVirt permissions
- Sandbox pod NetworkPolicy: ingress only from `app: cli-mcp-server` on 8090; egress only to K8s API (6443) + kube-dns (53); no internet
- Sandbox pods must carry the `tarsy.redhat.com/component: cli-mcp-sandbox` label — coordinate with the workload-analyzer team to whitelist this label so that investigation commands (which may contain security-related keywords like `xmrig`, `miner`, etc.) are not flagged as false positive abuse alerts (see [SANDBOX-1841](https://redhat.atlassian.net/browse/SANDBOX-1841))

**Note — workload-analyzer false positives:** The cli-mcp-server architecture avoids exec-ing into user containers (the sandbox agent runs commands in its own pod, and the investigation SA has no `pods/exec` permission). However, because the LLM has full bash access, investigation commands may contain security-related keywords that trigger the workload-analyzer's command-line rules. The sandbox pod labels should be used by the workload-analyzer to recognize these pods as trusted infrastructure and skip command-line scanning. This was identified after a false positive incident where `mcp-server-devsandbox`'s `podFind` tool triggered an abuser alert by running `find -name '*xmrig*'` inside a user container (see [SANDBOX-1841](https://redhat.atlassian.net/browse/SANDBOX-1841) for the broader investigation into smarter process rules).

**Depends on:** SANDBOX-1815

---

### ☐ SANDBOX-1819: TARSy integration

As a developer I want TARSy wired to the CLI MCP server so that investigation agents can use the bash tool and sessions are cleaned up afterwards.

Three PRs to the `tarsy` repo:
1. `custom_headers` support in `TransportConfig` with `{{.SESSION_ID}}` template resolution
2. Session cleanup call (`DELETE /sessions/{id}`) after investigation completes in `pkg/queue/executor.go`
3. `tarsy.yaml` entry for `cli-mcp-server` with custom_headers, tool instructions, data masking, and summarization config

**Acceptance Criteria**
- TARSy agents can invoke the `bash` tool and receive structured output
- `X-Session-ID` header is sent with every MCP request
- Sessions are cleaned up after investigation (idle-timeout as fallback)

**Depends on:** SANDBOX-1818

---

## Phase 4: Production Rollout

### ☐ SANDBOX-1820: Production rollout and observability

As a developer I want production overlays and custom Prometheus metrics so that the team can monitor and tune sandbox pod lifecycle.

**Acceptance Criteria**
- Production overlay with image tags, resource limits, replica count
- Custom metrics: pod creation latency, warm pool utilization, active sessions, command duration histogram
- Grafana dashboard for pod lifecycle (creation rate, cleanup rate, pool hit rate)

**Depends on:** SANDBOX-1818

---

### ☐ SANDBOX-1821: Documentation

As a developer I want AGENTS.md and an operational runbook so that AI coding agents and on-call engineers can work with the project effectively.

**Acceptance Criteria**
- `AGENTS.md` covers build/test commands, project structure, and key patterns (consistent with mcp-server-devsandbox)
- Runbook covers: pod stuck in Pending, stale cleanup not running, auth failures, warm pool not replenishing, agent unreachable

**Depends on:** SANDBOX-1820

---

## Dependency Graph

```
Phase 1 (Sandbox Agent):
  SANDBOX-1805 → SANDBOX-1806 → SANDBOX-1807 → SANDBOX-1808
                  SANDBOX-1806 → SANDBOX-1809
                                 SANDBOX-1807 → SANDBOX-1809

Phase 2 (MCP Server):
  SANDBOX-1805 → SANDBOX-1810 → SANDBOX-1811
                                 SANDBOX-1810 → SANDBOX-1813
  SANDBOX-1806 → SANDBOX-1812 → SANDBOX-1813
                                 SANDBOX-1813 → SANDBOX-1814 → SANDBOX-1815
  SANDBOX-1810..SANDBOX-1814 → SANDBOX-1816
  SANDBOX-1808 + SANDBOX-1815 → SANDBOX-1817

Phase 3 (Deploy + Integrate):
  SANDBOX-1815 → SANDBOX-1818 → SANDBOX-1819

Phase 4 (Production):
  SANDBOX-1818 → SANDBOX-1820 → SANDBOX-1821
```
