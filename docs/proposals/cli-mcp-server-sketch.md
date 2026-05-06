# CLI MCP Server — Sandboxed Exec Environment for LLM Investigation

**Status:** Sketch complete — ready for detailed design.

## Problem

TARSy's alert investigation agents are limited to pre-built, purpose-specific MCP tools. When an investigation requires a command not covered by existing tools (e.g., `oc adm top nodes`, `oc get clusteroperators`, `helm list`, or a multi-flag `kubectl get` with custom output formatting), the LLM is stuck — it cannot proceed without a human adding a new tool, rebuilding, and redeploying.

Today, six MCP servers provide structured tools:

| Server | Tools | Style |
|---|---|---|
| `kubernetes-mcp-server` (upstream) | K8s CRUD (get, list, logs, events) | Structured — native API calls, no kubectl binary |
| `mcp-server-devsandbox` (standard) | Pod inspection, file/grep in containers, guards | Structured — purpose-built per use case |
| `mcp-server-devsandbox` (virt) | VM SSH, VM lifecycle, VM inspection | Structured |
| `mcp-server-devsandbox` (actions) | VM start/stop (destructive) | Structured |
| `argocd-mcp-server` | Argo CD application analysis | Structured |
| `observability-mcp-server` | Loki/Prometheus queries | Structured |

Each new investigation pattern requires Go code → PR → review → build → deploy. The long tail of edge cases makes this unsustainable.

Beyond the coverage gap, existing structured tools fundamentally limit what the LLM can do. It cannot:

- Pipe output through utilities (`oc get pods -o json | jq '.items[].metadata.name'`)
- Chain commands (`oc get nodes && oc adm top nodes`)
- Write intermediate files for multi-step analysis
- Use standard Unix tools (`grep`, `awk`, `sort`, `wc`, `jq`) on command output
- Build up context incrementally across calls (environment variables, working directory, files)

Local coding agents (Claude Code, Cursor, OpenHands, OpenClaw) give developers all of these capabilities. The goal is to provide the same power to TARSy investigation agents, but in a sandboxed Kubernetes environment.

## Goal

A new MCP server that provides TARSy agents with a **sandboxed execution environment** — a per-investigation Kubernetes pod where the LLM has a persistent bash shell with full CLI access. Security comes from the sandbox boundary (RBAC, pod isolation, network isolation), not from application-level command filtering.

The server manages sandbox pod lifecycle: creating pods on demand, proxying shell commands into them via a lightweight agent binary, and cleaning up when investigations end.

**Initial scope:** `oc` and `kubectl`, plus standard Unix utilities (`jq`, `grep`, `awk`). The sandbox image is extensible to additional CLIs (e.g., `virtctl`, `helm`) by installing binaries — no server code changes.

This server **complements** existing structured MCP servers — it doesn't replace them. Structured tools remain better for well-understood, high-frequency operations. This server gives the LLM the flexibility to handle the long tail.

## How Local Agents Do It

Research across five major open-source coding agents shows a consistent pattern: a persistent shell session with full bash access, security enforced by environment boundaries rather than command filtering.

| Agent | Execution Model | Sandbox | Session State |
|---|---|---|---|
| **Claude Code** | Persistent bash session (`bash` tool). LLM sends any shell command, state persists between calls. | OS-level (user responsibility) | Stateful — env vars, cwd, files persist |
| **Cursor** | Terminal tool with workspace scoping. Agent runs commands freely; only asks approval for network access. | OS-level: Seatbelt (macOS), Landlock+seccomp (Linux). Workspace-scoped writes. | Stateful within workspace |
| **OpenHands** | Docker container per session. Agent binary inside container exposes REST API. Bash + browser + Jupyter plugins. | Docker container isolation. V1 supports Docker, K8s, and local runtimes. | Stateful — full container filesystem |
| **OpenClaw** | Single `exec` tool with host modes (sandbox, gateway, node). Foreground/background/TTY execution. | Configurable per host type. Rejects PATH/LD_* hijacking. | Stateful within workspace |
| **Goose** | Shell tool via developer extension. Plan + execute loop. | macOS sandbox (Seatbelt). BoxLite VMs proposed. | Stateful via extension |

**Common pattern:** A persistent shell where the LLM has full bash (pipes, redirects, chaining), a writable workspace for intermediate results, and security via environment boundaries — not command allowlists. OpenHands, the most mature open-source agent sandbox, uses an agent binary inside the container to manage the persistent session and expose it via API.

## Production Kubernetes Patterns

Several production-grade solutions exist for running sandboxed agent execution environments on Kubernetes:

| Solution | Architecture | Key Feature |
|---|---|---|
| **Azure Container Apps Dynamic Sessions** | Pre-warmed session pools with Hyper-V isolation. `launchShell` + `runShellCommandInRemoteEnvironment` tools. | Platform-managed sandbox with subsecond startup via warm pools |
| **mcp-sandboxd** | Maps conversation ID to long-running Docker/K8s container. `run_sandbox` tool. | Simple session→container mapping, artifact extraction via `/artifacts` |
| **ProDisco** | Per-session Sandbox CRD (Kata VM). gRPC proxy between MCP server and sandbox pod. | Kubernetes-native, per-session isolation, pre-configured K8s module inside sandbox |
| **K8s Agent Sandbox** (SIG Apps) | Sandbox CRD with gVisor/Kata runtimes. Warm pools, stable identity, lifecycle management. | Official Kubernetes primitive for agent workloads (v0.2.1, March 2026) |

These validate the per-session sandbox pod pattern and inform our design choices.

## Approach

A **sandboxed exec environment** — closer to what Claude Code, Cursor, and OpenHands offer, but running in a Kubernetes pod with strong isolation.

```
TARSy → bash(command) + X-Session-ID header → sandbox pod (agent + bash + CLIs + filesystem) → K8s
```

The MCP server manages sandbox pods directly via `client-go` (create/delete in the `tarsy` namespace, where the MCP server itself is deployed). Each sandbox pod runs a lightweight Go agent binary that manages a persistent bash session and exposes it via HTTP API. The MCP server communicates with the agent via the pod's IP address (obtained from the pod status after creation) — not via the K8s exec API — giving clean structured responses (exit code, stdout, stderr, timing) and true session persistence.

## How It Relates to the Existing System

### Relationship to existing MCP servers

```
TARSy
 ├── kubernetes-mcp-server          (structured K8s API tools — keep as-is)
 ├── mcp-server-devsandbox          (structured sandbox tools — keep as-is)
 ├── mcp-server-devsandbox-virt     (structured VM tools — keep as-is)
 ├── mcp-server-devsandbox-actions  (structured VM lifecycle — keep as-is)
 ├── argocd-mcp-server              (structured Argo CD tools — keep as-is)
 ├── observability-mcp-server       (structured Loki/Prom tools — keep as-is)
 └── cli-mcp-server (NEW)           (sandboxed exec environment)
```

The new server is additive. Agents that benefit from it gain it as an additional `mcp_servers` entry in `tarsy.yaml`. Existing structured tools remain the preferred path for their specific use cases.

### Relationship to `mcp-server-devsandbox`

| | mcp-server-devsandbox | cli-mcp-server (this) |
|---|---|---|
| **Tool model** | One tool per use case (`user-pods`, `grep-files-in-pod`) | Single `bash` tool |
| **What LLM controls** | Only tool parameters (namespace, pod name) | Full bash commands |
| **Shell support** | No — Go `exec.Command` only | Yes — pipes, redirects, chaining |
| **Filesystem** | Inspects existing pods | Ephemeral per-session workspace |
| **Adding capability** | Write Go code, add a tool, rebuild | Install binary in sandbox image |
| **Session model** | Stateless | Per-investigation sandbox pod |

### Deployment pattern

The MCP server itself follows the established deployment pattern (kube-rbac-proxy sidecar, ServiceAccount, NetworkPolicy, TLS cert). It requires two ServiceAccounts with distinct roles:

- **`cli-mcp-server` SA** — for the MCP server pod itself. Needs `system:auth-delegator` (for kube-rbac-proxy token review) and RBAC to manage sandbox pods in the `tarsy` namespace (`create`/`delete`/`list`/`get` pods).
- **`cli-mcp-investigation-sa` SA** — for the kubeconfig mounted into sandbox pods. Read-only investigation permissions only (`view`, `list-nodes`, `kube-investigation-readonly`). No `pods/exec`, no VM lifecycle.

The key difference from existing MCP servers: it also manages sandbox pods in the cluster.

```
TARSy
  ↓ (bearer token + X-Session-ID header per request)
kube-rbac-proxy (TLS :8443)
  ↓
cli-mcp-server (:8080)  ← "control plane"
  ├── /mcp            → MCP tool handlers (extract session ID from X-Session-ID header)
  ├── /sessions/{id}  → DELETE: session cleanup (called by TARSy, not LLM)
  ├── /metrics        → Prometheus
  ├── /live           → liveness
  └── /health         → readiness
       ↓
  Session Manager (client-go, tarsy namespace)
  ├── Extract session ID from X-Session-ID HTTP header
  ├── Create sandbox pod on first bash for a session ID
  ├── Discover sandbox pods by label (session-id)
  ├── Proxy shell commands to sandbox agent via HTTP (pod IP:8090)
  ├── Return structured response (stdout, stderr, exit code) to LLM
  └── Cleanup pod via DELETE /sessions/{id} or TTL expiry
       ↓
  Sandbox Pod (one per investigation session, in tarsy namespace)
  ├── sandbox-agent (Go binary, HTTP :8090, manages persistent bash session)
  ├── CLIs: oc, kubectl (future: helm, virtctl)
  ├── Utils: jq, yq, grep, awk, curl
  ├── /config/kubeconfig  (read-only mount, cli-mcp-investigation-sa)
  ├── /workspace/         (ephemeral, writable)
  └── NetworkPolicy: ingress from cli-mcp-server, egress to K8s API servers only
       ↓
  Target K8s clusters (via read-only kubeconfig)
```

## Key Concepts

### Sandbox pod

An ephemeral Kubernetes pod created for each investigation session. It contains:

- **Sandbox agent** — a lightweight Go HTTP server (~200-300 lines) that manages a persistent bash session. The pod's entrypoint. Accepts commands via HTTP, returns structured JSON responses (stdout, stderr, exit code, execution time). This is the same pattern used by OpenHands (agent binary inside container).
- **Pre-installed CLIs** — `oc`, `kubectl`, and future CLIs. The Containerfile for the sandbox image controls what's available.
- **Unix utilities** — `jq`, `yq`, `grep`, `awk`, `sort`, `wc`, `curl` (internal-only network). These make the LLM productive — it can pipe, filter, and transform output the same way a developer would.
- **Ephemeral workspace** — `/workspace` is an `emptyDir` volume. The LLM can write intermediate files during an investigation. Everything is destroyed when the pod is deleted.
- **Read-only kubeconfig** — mounted from a kubeconfig secret scoped to the dedicated investigation ServiceAccount. Contains contexts for all target clusters.
- **Pod security** — non-root, drop all capabilities, no privilege escalation. Resource limits (CPU/memory) enforced.

Adding a future CLI means installing the binary in the sandbox image. No server code changes.

### Sandbox agent

The lightweight Go binary running inside each sandbox pod. It:

1. Starts an HTTP server on a fixed port (e.g., `:8090`)
2. Spawns a persistent `bash` process (on first `/exec` request, or on startup if assigned at pod creation)
3. For each command request, pipes the command to bash's stdin, captures stdout/stderr
4. Returns a structured JSON response: `{"stdout": "...", "stderr": "...", "exit_code": 0, "duration_ms": 123}`
5. Maintains session state naturally — env vars, working directory, and aliases persist because the bash process stays alive
6. Exposes a `/health` endpoint for readiness checks
7. Exposes a `POST /assign` endpoint — accepts the auth token, transitions the agent from "unassigned" to "assigned" mode. Can only be called once. Required for warm pool pods; for pods created on demand, the token is delivered via `secretKeyRef` env var and `/assign` is not needed.

The agent is simple by design. It doesn't implement security filtering or cluster resolution — those concerns belong in the MCP server or the sandbox boundary. The agent just runs commands and returns results.

### Session manager

The component inside `cli-mcp-server` that manages sandbox pod lifecycle:

1. **Assign or create** — on first `bash` call for a session, assigns an available warm pod from the pool (if warm pool is enabled and a pod is available) or creates a new sandbox pod from a pod template via `client-go`. Labels the pod with the session ID extracted from the `X-Session-ID` HTTP header (set by TARSy, typically the investigation ID). Delivers the per-session HMAC auth token to the agent via `POST /assign`. Records the pod IP for agent communication. See [Session routing via HTTP header](#session-routing-via-http-header) for how the session ID reaches the MCP server.
2. **Discover** — finds sandbox pods by label. Uses an in-memory cache (TTL ~30s) to avoid per-request K8s API calls. On MCP server restart, rediscovers all active sandbox pods by label in a single list call and rebuilds the pod IP cache.
3. **Execute** — sends the shell command to the sandbox agent's HTTP API (`http://<pod-ip>:8090/exec`) and returns the structured response as-is. Output treatment (summarization, masking) is TARSy's responsibility, not the MCP server's.
4. **Cleanup** — deletes the sandbox pod and its auth Secret when TARSy calls `DELETE /sessions/{id}` (a plain HTTP endpoint, not an MCP tool) or the TTL expires (idle timeout). Session lifecycle is managed by TARSy, not the LLM. If the sandbox agent crashes, the pod restarts (standard K8s restart policy) and a new bash session begins — the MCP server detects this via the agent's `/health` endpoint and informs the LLM that session state was reset.
5. **Replenish pool** — when a warm pod is assigned to a session, a background goroutine creates a replacement to maintain the configured pool size.

Pod labels are the source of truth for session→pod mapping. The session ID is provided by TARSy via an HTTP header on every request — the LLM never sees or manages it. This keeps the MCP server stateless — any replica can serve any request, and restarts don't lose session routing.

### Warm pod pool

An optional pool of pre-created sandbox pods that are ready to be assigned to sessions immediately, eliminating cold start latency (pod scheduling, image pull, container startup, bash process initialization).

Configured via `--warm-pool-size N` (default 0 = disabled). When enabled:

- **Warm pods** are created without a session ID label and without an auth token. The sandbox agent starts in "unassigned" mode — `/health` returns 200, `/exec` returns 503.
- **Assignment** is atomic: the session manager picks a warm pod, creates the per-session auth Secret, calls `POST /assign` on the agent (delivering the auth token), and patches the pod labels to add the session ID.
- **Replenishment** is asynchronous — after a warm pod is assigned, a background reconciler creates a replacement to maintain the target pool size.
- **Multiple MCP server replicas** coordinate via Kubernetes labels: each replica lists unassigned pods and attempts to claim one by patching its session ID label. The first patch wins; concurrent attempts fail gracefully and retry with a different pod.

When the pool is exhausted or disabled (`--warm-pool-size 0`), the session manager falls back to creating pods on demand — the current behavior.

### MCP tool: `bash`

The name `bash` is the most common tool name for shell execution across LLM agent frameworks, making it immediately recognizable. It avoids collision with `exec` (which in K8s/Docker means "attach to a running container") and accurately describes the sandbox: a persistent bash process.

A single MCP tool that gives the LLM full bash access in the sandbox:

| Parameter | Type | Required | Description |
|---|---|---|---|
| `command` | string | yes | Shell command to execute (full bash — pipes, redirects, chaining supported) |
| `timeout` | int | no | Max execution time in seconds (default 60, max 300) |

Session routing is handled transparently via the `X-Session-ID` HTTP header — the LLM never needs to manage session identity. See [Session routing via HTTP header](#session-routing-via-http-header).

The LLM writes bash commands the same way a developer would. Multi-cluster targeting uses `--context=<cluster>` per command — explicit, self-documenting, and supports cross-cluster investigation naturally:

```
bash(command="oc get clusteroperators --context=rm1")
bash(command="oc get pods -n openshift-ingress --context=rm1 -o json | jq '.items[] | {name: .metadata.name, ready: .status.containerStatuses[0].ready}'")
bash(command="oc adm top nodes --context=rm1 --sort-by=cpu | head -5")
bash(command="diff <(oc get pods --context=rm1 -o name) <(oc get pods --context=rm2 -o name)")
bash(command="export CTX=rm1; for ns in $(oc get ns --context=$CTX -o name | head -10); do echo \"=== $ns ===\"; oc get pods --context=$CTX -n ${ns#namespace/} --no-headers 2>/dev/null | wc -l; done")
```

Working directory and environment variables persist between calls within the same session — maintained by the persistent bash process in the sandbox agent.

**Note on future write capabilities:** If the server later adds write-capable CLIs (e.g., `helm install`, `oc apply`), per-cluster sandbox pods should be considered. With write access, targeting the wrong cluster has destructive consequences, and per-cluster isolation eliminates that risk. For read-only investigation, a shared kubeconfig with all contexts is safe.

## Security Model

Security comes from the sandbox boundary, not from application-level command filtering. No allowlists, no blocklists. This matches how production sandbox environments (Cursor, OpenHands, Azure Container Apps) enforce safety.

### Layer 1: Kubernetes RBAC (hard boundary)

The kubeconfig mounted into the sandbox pod uses a **dedicated ServiceAccount** (`cli-mcp-investigation-sa`) with only investigation-relevant ClusterRoles:

- `view` ClusterRole — standard K8s read-only for namespaced resources (explicitly excludes Secrets)
- `list-nodes` ClusterRole — node/machine listing
- `kube-investigation-readonly` ClusterRole — cluster-scoped reads (namespaces, PVs, CRDs, metrics, OpenShift operators, machine configs)

This SA explicitly **does not** include `pods/exec` or KubeVirt lifecycle permissions (which are present on `sandbox-mcp-sa` for `mcp-server-devsandbox`). Since the sandbox gives the LLM full shell access with no allowlist, the SA must be tightly scoped — the LLM can exercise any permission the SA has.

### Layer 2: Pod isolation

Each investigation gets its own sandbox pod. No cross-session filesystem access. No shared state between investigations.

### Layer 3: Agent authentication (HMAC token)

NetworkPolicy alone is not sufficient — CNI bugs, misconfigurations, or a compromised pod in the same namespace could bypass it. As defense-in-depth, the sandbox agent requires a bearer token on every request.

The token is derived deterministically using HMAC: `token = HMAC-SHA256(shared_secret, session_id)`. All MCP server replicas share the same HMAC key (a single Kubernetes Secret). At pod creation, the MCP server computes the token, stores it in a per-session Kubernetes Secret, and the sandbox pod reads it via `secretKeyRef` — keeping the token out of the pod spec. At command execution, any replica computes the same token from the session ID (available in the `X-Session-ID` header) and includes it in the HTTP request to the agent. The sandbox agent verifies the token on every `/exec` request.

This is fully stateless — no token lookup, no shared database. Same input always produces the same token.

### Layer 4: Network isolation

A `NetworkPolicy` on sandbox pods enforces:
- **Ingress:** allow only from `cli-mcp-server` pods (so the MCP server can reach the sandbox agent's HTTP port)
- **Egress:** allow only to the Kubernetes API servers of target clusters. No internet access. No access to other services unless explicitly needed.

This prevents data exfiltration, limits blast radius, and ensures sandbox pods are only reachable by the MCP server.

### Layer 5: Ephemeral storage

The `/workspace` filesystem is an `emptyDir` — it dies with the pod. No persistent state between investigations. No data leakage across sessions.

### Layer 6: Resource limits and TTL

- **Resource limits** — CPU/memory limits per sandbox pod prevent resource abuse
- **TTL** — pods auto-deleted after configurable idle timeout (e.g., 30 minutes)
- **Command timeout** — individual commands killed after configurable max (default 60s, max 300s)
- **Data masking** — TARSy's existing `data_masking` config scrubs tokens, certs, emails
- **Summarization** — TARSy's existing `summarization` config compresses large responses

## TARSy Integration

### Session routing via HTTP header

The session ID that maps `bash` calls to sandbox pods is **not** a tool parameter — the LLM never sees or manages it. Instead, TARSy injects the session ID (typically the investigation ID) as an `X-Session-ID` HTTP header on every MCP request, and the MCP server extracts it server-side via the MCP SDK's `RequestExtra.Header`.

**Why not a tool parameter?** Three reasons:

1. **LLM reliability** — Removing `session_id` from the tool schema eliminates an entire class of errors (hallucinated IDs, typos, inconsistent values across calls). The LLM just calls `bash(command="...")`.
2. **Stateless replicas** — `cli-mcp-server` runs as a stateless deployment behind a Kubernetes Service. The MCP Streamable HTTP transport doesn't guarantee session affinity, so each request may hit a different replica. The header travels with every request, so any replica can route correctly.
3. **Separation of concerns** — Infrastructure routing is TARSy's job, not the LLM's. The LLM focuses on writing bash commands; TARSy ensures they reach the right sandbox pod.

**How it works:**

1. TARSy creates a per-session MCP client (via `ClientFactory`) with a custom HTTP round-tripper that injects `X-Session-ID: <investigation-id>` on every request — the same pattern already used for bearer token injection.
2. Every MCP request (tool calls, notifications) carries the header automatically.
3. On the server side, `cli-mcp-server` reads `req.Extra.Header.Get("X-Session-ID")` in each tool handler and routes to the corresponding sandbox pod.
4. If the header is missing, the server rejects the request with a clear error.

This is a first-class capability of the MCP Go SDK — `RequestExtra` (populated by `StreamableHTTPHandler`) includes the full `http.Header` from each incoming request.

### tarsy.yaml configuration

```yaml
mcp_servers:
  cli-mcp-server:
    transport:
      type: "http"
      url: "https://cli-mcp-server:8443/mcp"
      bearer_token: "{{.CLI_MCP_BEARER_TOKEN}}"
      timeout: 90
      verify_ssl: false
      custom_headers:
        X-Session-ID: "{{.SESSION_ID}}"  # Injected per-session by TARSy (investigation ID)
    instructions: |
      This server provides a sandboxed shell environment for cluster investigation.
      You have full bash access: pipes, redirects, chaining, and standard Unix tools (jq, grep, awk).
      Available CLIs: oc, kubectl.
      Available clusters: rm1, rm2, rm3. Use --context=<cluster> to target a specific cluster.
      Your workspace at /workspace is ephemeral — use it for intermediate files during investigation.
      All cluster access is read-only via RBAC.
    data_masking:
      enabled: true
      pattern_groups: ["kubernetes", "security"]
      patterns: ["certificate", "token", "email"]
    summarization:
      enabled: true
      summary_max_token_limit: 1200
```

The `custom_headers` field is a new addition to TARSy's transport config. Headers support the same Go template interpolation as other config values. `SESSION_ID` is resolved per-session by the MCP client factory when creating the HTTP client — it's not a static environment variable.

### Agent wiring

Added to investigation agents that benefit from flexible CLI access. Which agents get access is a TARSy configuration decision — the `mcp_servers` list per agent in `tarsy.yaml`. The server has no knowledge of which agent is calling it.

### Required TARSy changes

This MCP server depends on two TARSy capabilities that don't exist yet:

1. **`custom_headers` in `TransportConfig`** — TARSy's `config.TransportConfig` struct needs a new `CustomHeaders map[string]string` field. Headers with template variables (like `{{.SESSION_ID}}`) must be resolved per-session, not at config load time. This requires changes to `config.TransportConfig`, `mcp.createHTTPTransport`, and `mcp.ClientFactory` — the factory must accept per-session template variables and create HTTP clients with a round-tripper that injects the resolved headers (the same pattern used for bearer token injection).

2. **Session cleanup HTTP call** — After an investigation chain completes, TARSy needs to call `DELETE /sessions/{id}` on the MCP server. Today, TARSy only communicates with MCP servers via the MCP protocol (`ClientFactory` → `Client`). Calling a plain REST endpoint requires a separate HTTP client constructed from the same transport config (URL base, bearer token, TLS settings). This is a new code path in the session executor. As a fallback, the TTL safety net on the MCP server handles cleanup even if TARSy doesn't call DELETE — so this integration can be deferred to a follow-up if needed.

## Technology

- **Go** — consistent with all existing MCP servers
- **`github.com/modelcontextprotocol/go-sdk/mcp`** — MCP SDK
- **`github.com/codeready-toolchain/mcp-common`** — shared metrics/logging middleware
- **`github.com/spf13/cobra`** — CLI flag parsing
- **`k8s.io/client-go`** — Kubernetes client for sandbox pod management (create, delete, list)

Two binaries in the same repo, two container images:
- `cmd/cli-mcp-server/` — the MCP server (control plane). Image: multi-stage Go build, standard deployment with kube-rbac-proxy sidecar.
- `cmd/sandbox-agent/` — the lightweight agent running inside sandbox pods. Image: `quay.io/codeready-toolchain/oc-client-base-minimal` base + agent binary + Unix utilities (jq, yq, grep, awk, curl).

Both deployed in the `tarsy` namespace.

## What Is Out of Scope

- **Replacing existing structured MCP servers** — this is additive
- **Write/mutate operations against clusters** — read-only investigation by design (RBAC-enforced)
- **Interactive commands requiring stdin** — no `oc edit`, no interactive `oc exec -it`
- **Multi-tenancy / per-user credentials** — uses shared kubeconfig (per-user auth is a separate proposal)
- **Custom output parsing** — the server returns raw shell output; TARSy's summarization handles compression
