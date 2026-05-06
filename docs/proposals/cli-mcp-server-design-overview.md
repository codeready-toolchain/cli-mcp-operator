# CLI MCP Server — Design Overview

**Status:** Final

**Related documents:**
- [Sketch](cli-mcp-server-sketch.md) — problem statement, approach selection, high-level decisions
- [Detailed Design](cli-mcp-server-design.md) — Go types, implementation details, code samples

---

## What it is

A new MCP server that gives TARSy investigation agents a **sandboxed shell environment** — an isolated Kubernetes pod where the LLM has a persistent bash session with full CLI access. Think of it as giving the LLM the same capabilities a developer has in a terminal, but running inside a locked-down container.

The server exposes a single MCP tool:
- **`bash`** — run any shell command in the sandbox

Session cleanup is handled by TARSy via a plain HTTP endpoint (`DELETE /sessions/{id}`), not by the LLM.

## Why it exists

Today, TARSy agents can only use pre-built, structured MCP tools. When an investigation needs a command not covered by an existing tool (e.g., `oc adm top nodes`, piping output through `jq`, or comparing resources across clusters), the LLM is stuck. Adding each new command requires Go code, PR review, build, and deploy.

Beyond the coverage gap, structured tools fundamentally limit what the LLM can do. It cannot pipe output through utilities, chain commands, write intermediate files, or build up context across calls. Local coding agents (Claude Code, Cursor, OpenHands) give developers all of these capabilities. This server brings the same power to TARSy, but in a sandboxed Kubernetes environment.

This server **complements** existing structured MCP servers — it doesn't replace them. Structured tools remain better for well-understood, high-frequency operations. This server handles the long tail.

## How it works

### Two components

**1. The MCP server** (control plane) — a stateless Go binary deployed as a standard MCP server (kube-rbac-proxy, TLS, metrics). Its job is managing sandbox pods and proxying commands. It doesn't parse, validate, or transform shell commands.

**2. The sandbox agent** (data plane) — a lightweight Go binary (~200-300 lines) that runs inside each sandbox pod. It manages a persistent bash session and exposes a simple HTTP API: `POST /exec` to run commands and `GET /health` for readiness checks.

### The flow

1. TARSy calls `bash(command="oc get pods -n foo --context=rm1")` — the `X-Session-ID` header is automatically attached by TARSy's MCP client
2. The MCP server extracts the session ID from the `X-Session-ID` HTTP header and looks up the sandbox pod using Kubernetes labels
3. If no pod exists, it creates one and waits for it to be ready
4. It sends the command to the sandbox agent via HTTP
5. The agent pipes the command into the persistent bash process, captures stdout/stderr, and returns a structured JSON response (output, exit code, execution time)
6. The MCP server returns the result as-is to TARSy. Output treatment (summarization, masking) is TARSy's responsibility.

### Session lifecycle

Each investigation gets its own sandbox pod. If a warm pool is configured (`--warm-pool-size N`), a pre-warmed pod is assigned instantly; otherwise, a new pod is created on the first `bash` call. The pod persists for the duration of the investigation. Environment variables, working directory, and files persist between calls because the bash process stays alive.

When the investigation is done:
- **Primary cleanup:** TARSy calls `DELETE /sessions/{id}` on the MCP server — a plain HTTP endpoint, not an MCP tool. This immediately deletes the sandbox pod. The LLM is not involved in session cleanup.
- **Fallback cleanup:** A TTL safety net deletes pods that have been idle for 30 minutes, catching cases where TARSy crashes or the DELETE call fails.

## Key design decisions

### 1. Stateless server with header-based session routing

The MCP server runs in **stateless mode**, consistent with all other MCP servers. Session routing uses the `X-Session-ID` HTTP header, which TARSy injects automatically on every MCP request (typically set to the investigation ID). The server routes to the correct sandbox pod by looking up Kubernetes labels — no in-memory session tracking, no sticky sessions. Any server replica can handle any request.

The session ID is **not** a tool parameter — the LLM never sees or manages it. This eliminates an entire class of errors (hallucinated IDs, typos, inconsistent values across calls) and keeps the tool schema minimal. TARSy's MCP client injects the header via a custom HTTP round-tripper, the same pattern used for bearer token injection.

### 2. Security from the sandbox boundary

There are no application-level command allowlists or blocklists. Security comes from the sandbox itself:

- **RBAC** — the kubeconfig mounted in the sandbox pod uses a dedicated ServiceAccount (`cli-mcp-investigation-sa`) with read-only permissions. No `pods/exec`, no write operations, no VM lifecycle.
- **Pod isolation** — each investigation gets its own pod. No cross-session filesystem access.
- **Agent authentication** — the sandbox agent requires a bearer token on every request, derived via `HMAC-SHA256(shared_secret, session_id)`. All MCP server replicas share the HMAC key and can compute the correct token statelessly. The token is delivered to the sandbox pod via a per-session Kubernetes Secret (not a plaintext env var), keeping it out of the pod spec. This prevents unauthorized command execution even if NetworkPolicy is bypassed.
- **Network isolation** — a NetworkPolicy restricts sandbox pods to only reach Kubernetes API servers. No internet access, no access to other services.
- **Ephemeral storage** — the `/workspace` filesystem is an `emptyDir` volume that is destroyed when the pod is deleted. No data persists between investigations.
- **Resource limits** — CPU/memory limits per sandbox pod prevent resource abuse.

### 3. Pipe-based bash session with delimiter protocol

The sandbox agent manages a persistent `bash` process using Go's standard library pipes (stdin/stdout/stderr). Each command is wrapped with UUID-based delimiters to isolate its output from the continuous stream. This gives clean stdout/stderr separation (important for structured responses) without needing PTY complexity, which adds no value for LLM consumption.

If the bash process crashes (OOM, signal), the agent respawns it on the next request and notifies the LLM that session state was reset.

### 4. Hardcoded pod spec with CLI flag overrides

The sandbox pod specification is built in Go code. Variable parts (container image, resource limits, namespace, idle timeout) are exposed as CLI flags, consistent with how `mcp-server-devsandbox` handles configuration. This can evolve to ConfigMap overrides later if operators need more flexibility.

### 5. Configurable warm pod pool

An optional pool of pre-created sandbox pods eliminates cold start latency. Configured via `--warm-pool-size N` (default 0 = disabled, create on demand).

When enabled, the session manager maintains N unassigned pods that are fully booted (image pulled, container running, bash process ready) but have no session ID or auth token. On first `bash` call for a new session, the manager assigns a warm pod by delivering the HMAC auth token via `POST /assign` on the agent and patching the pod labels. A background reconciler replenishes the pool after each assignment.

Multiple MCP server replicas coordinate via Kubernetes labels — each replica claims a warm pod by patching its session ID label (first writer wins). When the pool is exhausted, the manager falls back to creating pods on demand.

## Architecture diagram

```
TARSy
  ↓ (bearer token + X-Session-ID header per request)
kube-rbac-proxy (TLS :8443)
  ↓
cli-mcp-server (:8080, stateless, N replicas)
  ├── bash handler
  │     ├── Extract session ID from X-Session-ID header
  │     ├── Find sandbox pod by label (session ID)
  │     ├── Create pod if missing (client-go)
  │     └── Proxy command to sandbox agent (HTTP)
  ├── DELETE /sessions/{id} → delete sandbox pod (called by TARSy, not LLM)
  └── Stale pod cleanup (every 5 min, TTL 30 min)
       ↓ HTTP (pod IP:8090)
  Sandbox Pod (one per investigation, tarsy namespace)
  ├── sandbox-agent → persistent bash session
  ├── CLIs: oc, kubectl
  ├── Utils: jq, yq, grep, awk, curl
  ├── /config/kubeconfig (read-only, investigation SA)
  ├── /workspace/ (ephemeral, writable)
  └── NetworkPolicy: ingress from MCP server, egress to K8s API only
       ↓
  Target K8s clusters (via read-only kubeconfig)
```

## MCP tools

### `bash`

| Parameter | Type | Required | Description |
|---|---|---|---|
| `command` | string | yes | Shell command to execute (full bash — pipes, redirects, chaining) |
| `timeout` | int | no | Max execution time in seconds (default 60, max 300) |

Session routing is handled transparently via the `X-Session-ID` HTTP header — the LLM never needs to manage session identity.

Example usage:

```
bash(command="oc get clusteroperators --context=rm1")

bash(command="oc get pods -n openshift-ingress --context=rm1 -o json | jq '.items[] | {name: .metadata.name, ready: .status.containerStatuses[0].ready}'")

bash(command="diff <(oc get pods --context=rm1 -o name) <(oc get pods --context=rm2 -o name)")

bash(command="export CTX=rm1; for ns in $(oc get ns --context=$CTX -o name | head -10); do echo \"=== $ns ===\"; oc get pods --context=$CTX -n ${ns#namespace/} --no-headers 2>/dev/null | wc -l; done")
```

Non-zero exit codes are returned as tool results (not MCP errors) — a command returning "NotFound" is still useful output for the LLM.

### Session cleanup: `DELETE /sessions/{id}`

A plain HTTP endpoint (not an MCP tool) for session lifecycle management. TARSy calls this when the investigation chain completes — the LLM is not involved.

The endpoint deletes the sandbox pod by label and frees resources. The `{id}` path parameter is the session/investigation ID.

This separation is intentional: MCP tools are for investigation (the LLM uses them), lifecycle management is TARSy's job (via HTTP).

## ServiceAccounts and RBAC

Two ServiceAccounts with distinct roles:

| ServiceAccount | Used by | Permissions |
|---|---|---|
| `cli-mcp-server` | MCP server pod | Manage sandbox pods in `tarsy` namespace (`create`, `delete`, `get`, `list`, `watch`, `patch` pods) |
| `cli-mcp-investigation-sa` | Sandbox pods (via kubeconfig) | Read-only cluster access: `view` ClusterRole, `list-nodes`, `kube-investigation-readonly`. No `pods/exec`, no write operations. |

The MCP server SA is scoped to the `tarsy` namespace. The investigation SA is tightly scoped because the LLM has full shell access — it can exercise any permission the SA has.

## Error handling

| Scenario | Behavior |
|---|---|
| Pod creation fails | MCP error returned, session manager retries once |
| Agent unreachable | MCP error, cache entry invalidated, next request retries |
| Bash process crashes | Agent respawns bash, notifies LLM that session state was reset |
| Command timeout | Agent kills command, returns partial output with exit code 137 |
| Pod evicted | Next request creates new pod, LLM sees "new session" |
| K8s API unreachable | MCP error, health check fails |

## Deployment

The MCP server follows the established deployment pattern (kube-rbac-proxy sidecar, serving cert, NetworkPolicy). It is deployed in the `tarsy` namespace alongside existing MCP servers.

Two container images:
- **MCP server image** — minimal (distroless), contains only the server binary. No CLIs needed.
- **Sandbox agent image** — based on `oc-client-base-minimal`, includes the agent binary, `oc`, `kubectl`, and Unix utilities (`jq`, `yq`, `curl`, `grep`, `awk`).

Adding a future CLI (e.g., `virtctl`, `helm`) means installing the binary in the sandbox agent image — no server code changes.

### TARSy configuration

The server is added as an `mcp_servers` entry in `tarsy.yaml` with a `custom_headers` field that injects `X-Session-ID` per-session. Agent instructions tell the LLM how to use the sandbox and which clusters are available — the LLM doesn't need to know about session routing.

## Implementation phases

1. **Sandbox agent** — persistent bash session, HTTP API, Containerfile
2. **MCP server core** — session manager, pod lifecycle, tool handlers, Containerfile
3. **Deployment** — kustomize manifests, RBAC, NetworkPolicy, TARSy integration
4. **Production rollout** — monitoring, tuning, additional CLIs based on usage

## What is out of scope

- Replacing existing structured MCP servers — this is additive
- Write/mutate operations against clusters — read-only investigation by design
- Interactive commands requiring stdin — no `oc edit`, no interactive `oc exec -it`
- Multi-tenancy / per-user credentials — uses shared kubeconfig

## Future considerations

If the server later adds write-capable CLIs (e.g., `helm install`, `oc apply`), per-cluster sandbox pods should be considered. With write access, targeting the wrong cluster has destructive consequences, and per-cluster isolation eliminates that risk. For read-only investigation, a shared kubeconfig with all contexts is safe.

## Design decisions summary

| # | Topic | Decision | Rationale |
|---|---|---|---|
| 1 | Session identification | `X-Session-ID` HTTP header (injected by TARSy) | Server stays stateless. LLM never manages session IDs — eliminates hallucination/typo errors. Header travels with every request, works with any replica. |
| 2 | Session cleanup | `DELETE /sessions/{id}` endpoint (called by TARSy) + TTL safety net (30m) | Lifecycle management is TARSy's job, not the LLM's. Plain HTTP endpoint keeps MCP tools focused on investigation. TTL catches edge cases. |
| 3 | Pod template source | Hardcoded Go struct + CLI flags | Simplest approach, consistent with existing MCP servers. Can evolve to ConfigMap overrides later. |
| 4 | Bash session management | Pipe-based with UUID delimiter protocol | Clean stdout/stderr separation. No external dependencies. Well-known pattern used by OpenHands and Claude Code. |
| 5 | Tool naming | `bash` (not `shell_exec` or `exec`) | Most common shell tool name across LLM agent frameworks — maximally familiar to LLMs. Avoids collision with `exec` (which in K8s/Docker means "attach to a running container"). |
| 6 | Output handling | Return as-is, no truncation | MCP server is a dumb proxy. Output treatment (masking, summarization) is TARSy's responsibility via existing per-server config. |
| 7 | Sandbox agent auth | HMAC-derived bearer token per session | Defense-in-depth: even if NetworkPolicy is bypassed, agent rejects unauthenticated requests. HMAC is deterministic — any replica computes the same token. No token storage needed. |
| 8 | Warm pod pool | Configurable pool size (`--warm-pool-size`, default 0) | Eliminates cold start latency when enabled. Pool size 0 means create on demand (simplest). Warm pods coordinate across replicas via label-based claiming. |
