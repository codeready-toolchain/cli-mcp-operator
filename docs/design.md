# CLI MCP Server — Detailed Design

**Status:** Implemented (as-built)

**Related:** [Sketch](sketch.md) · [Architecture Overview](architecture-overview.md) · [README](../README.md)

## Design principles

1. **Stateless server, stateful sandbox** — durable routing state is in Kubernetes labels; bash state lives in the pod
2. **Sandbox as security boundary** — no command allowlists; RBAC, isolation, NetworkPolicy, and HMAC auth do the work
3. **Dumb proxy** — the server creates/routes/proxies; it does not parse or rewrite shell
4. **Two binaries** — control plane (`cli-mcp-server`) and data plane (`sandbox-agent`)

## Components

```
┌─────────────────────────────────────────────────────────┐
│  cli-mcp-server (Deployment, N replicas)                │
│  kube-rbac-proxy :8443 → server :8080                   │
│    /mcp              MCP tools (bash)                   │
│    DELETE /sessions  session teardown                   │
│    /metrics /live /health                               │
│    Session manager → client-go + agent HTTP client      │
└───────────────────────────┬─────────────────────────────┘
                            │ POST /exec (HMAC bearer)
                            ▼
┌─────────────────────────────────────────────────────────┐
│  Sandbox pod (one per session, optional warm pool)      │
│  sandbox-agent :8090 → persistent bash                  │
│  CLIs from image · /workspace emptyDir · kubeconfig     │
│  Labels: session-id, component=cli-mcp-sandbox          │
└───────────────────────────┬─────────────────────────────┘
                            ▼
                   Target APIs (e.g. K8s)
```

| Area | Responsibility |
|---|---|
| `pkg/tools` | MCP `bash` handler; reads `X-Session-ID` |
| `pkg/session` | Pod create/claim/cleanup, warm pool, pod IP cache |
| `pkg/agent` | HTTP client to sandbox agent (`/exec`, `/assign`, `/health`) |
| `pkg/sandbox` | Agent handlers + persistent bash session |
| `pkg/server` | MCP server wiring and HTTP mux |

## Request flow

1. Client calls `bash` with `X-Session-ID`
2. Server resolves the sandbox pod (cache → label lookup → warm claim or create)
3. Server `POST`s to `http://<pod-ip>:8090/exec` with an HMAC bearer token
4. Agent runs the command in the persistent bash process
5. Result (`stdout`, `stderr`, `exit_code`, `duration_ms`) returns as-is

Non-zero exit codes are tool results, not transport failures.

## Session routing and lifecycle

| Concern | Behavior |
|---|---|
| Identity | `X-Session-ID` header (RFC 1123 DNS label) |
| Source of truth | Pod labels (`tarsy.redhat.com/session-id`) |
| Cache | In-memory pod IP cache; rebuilds via label list after restart |
| Create | On-demand pod + per-session auth Secret, or claim from warm pool |
| Activity | `last-activity` annotation updated on each `bash` call |
| Teardown | `DELETE /sessions/{id}` deletes pod, auth Secret, cache entry |
| Idle GC | Periodic cleanup after `--idle-timeout` (default 30m) |

Warm pool (`--warm-pool-size`, default `0`): unassigned pods start without a token (`/exec` → 503). Assignment patches the session label, creates the auth Secret, and calls `POST /assign`. Replicas coordinate by label claim (first writer wins). Pool replenishes in the background.

## Sandbox agent

| Endpoint | Auth | Role |
|---|---|---|
| `POST /exec` | Bearer (HMAC) | Run command in persistent bash |
| `POST /assign` | None (once) | Deliver token to a warm pod |
| `GET /health` | None | Readiness |

Token delivery:

- **On-demand:** `SANDBOX_AUTH_TOKEN` from per-session Secret (`secretKeyRef`)
- **Warm pool:** `POST /assign` once; later calls return 409

Bash is a long-lived process (`bash --norc --noprofile`) with pipe I/O and a delimiter protocol so each command’s stdout/stderr/exit code stay isolated. Timed-out commands are killed without destroying the shell; if bash dies, the next request respawns it and reports that session state was reset.

## Security model

| Layer | What it does |
|---|---|
| Investigation RBAC | Read-only SA/kubeconfig mounted into sandbox pods |
| Pod isolation | One pod per session; non-root, no privilege escalation, caps dropped |
| Agent auth | `HMAC-SHA256(shared_secret, session_id)` bearer on `/exec` |
| Network | Ingress from MCP server; egress limited to intended APIs |
| Ephemeral storage | `/workspace` is `emptyDir` — gone with the pod |
| Limits | CPU/memory, command timeout, idle TTL |

The server does not filter commands. Capability is image contents + RBAC + network policy.

Two ServiceAccounts:

| SA | Used by | Purpose |
|---|---|---|
| `cli-mcp-server` | MCP server pod | Manage sandbox pods/secrets in the sandbox namespace |
| `cli-mcp-investigation-sa` | Sandbox pods (via kubeconfig) | Read-only investigation access |

## Error handling

| Scenario | Behavior |
|---|---|
| Missing/invalid session ID | Reject request |
| Pod create / agent unreachable | MCP error; cache invalidated; retry on next call |
| Bash crash | Respawn; stderr notes state reset |
| Command timeout | Kill command; return partial output / timeout exit |
| Pod evicted | Next call creates or claims a new pod |
| No explicit DELETE | Idle TTL eventually removes the pod |

## Deployment notes

- Server image: minimal (server binary only)
- Agent image: `Containerfile.agent` — agent + CLIs (`oc`/`kubectl`, `jq`, `yq`, `curl`, …)
- Typical deploy: `tarsy` namespace, kube-rbac-proxy sidecar, NetworkPolicy
- Adding CLIs = image change + `--sandbox-image`; no server code changes
- Client config (e.g. TARSy): inject `X-Session-ID`, call `DELETE /sessions/{id}` on finish, describe available CLIs/contexts in server instructions

## Out of scope

Same as the [sketch](sketch.md): additive to structured MCP servers; read-only investigation by design; no interactive stdin; no per-user credentials in this design.
