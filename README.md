# cli-mcp-server

MCP server that provides AI agents with a sandboxed bash shell inside per-investigation Kubernetes pods.

## Architecture

Two binaries in one repo:

- **cli-mcp-server** (`cmd/server/`) — Control plane. Manages sandbox pod lifecycle via client-go, exposes the `bash` MCP tool, proxies commands to sandbox agents.
- **sandbox-agent** (`cmd/agent/`) — Data plane. Runs inside each sandbox pod, manages a persistent bash process with `oc`/`kubectl`/`jq`/`yq`/`curl`.

## Project Structure

```
cli-mcp-server/
├── cmd/
│   ├── server/main.go          # MCP server entry point
│   └── agent/main.go           # Sandbox agent entry point
├── pkg/
│   ├── session/                 # Pod lifecycle, warm pool, cache
│   ├── agent/                   # HTTP client + shared types
│   ├── sandbox/                 # Bash session + HTTP handlers
│   ├── server/                  # MCP server setup
│   ├── tools/                   # MCP tool handlers (bash)
│   └── version/                 # Build version info
├── docs/
│   ├── proposals/               # Design documents
│   └── implementation/          # Implementation plan and Jira stories
├── Makefile
└── go.mod
```

## Build

```bash
make build          # Build both server and agent
make build-server   # Build only the MCP server
make build-agent    # Build only the sandbox agent
make test           # Run tests
make lint           # Run linter
make build-prod     # Production build (static, CGO disabled)
```

## Session & Pod Lifecycle

Each investigation gets its own sandbox pod. The MCP server is stateless — session
state lives in Kubernetes (pod labels/annotations) and is cached in memory for performance.

### Request Flow

```
TARSy (LLM)
  │  MCP tool call + X-Session-ID header
  ▼
MCP Server (stateless, multi-replica)
  │  HTTP POST /exec + Bearer HMAC token
  ▼
Sandbox Pod (per-session, persistent bash)
  │  stdout / stderr / exit_code
  ▼
MCP Server → TARSy
```

### GetOrCreatePod — Three-Tier Lookup

When a command arrives for a session, `SessionManager.GetOrCreatePod` resolves the
target pod using a three-tier strategy:

| Tier | Method | Latency | Description |
|------|--------|---------|-------------|
| 1 | **PodCache** | ~0ms | In-memory map of session ID → pod IP with 30s TTL |
| 2 | **Label discovery** | ~50-100ms | `List` pods by `tarsy.redhat.com/session-id` label |
| 3 | **Create** | ~3-8s | Create auth Secret + Pod, wait for readiness probe |

Cache misses fall through to tier 2; if no pod exists, tier 3 creates one
idempotently (handles `AlreadyExists`).

### Warm Pool

When `--warm-pool-size N` is set, N pre-warmed pods are maintained in a ready state.
New sessions are assigned a warm pod instantly (~0ms) instead of waiting for a cold
start. Disabled by default (`N=0`).

### Pod Spec

Each sandbox pod runs with a hardened security context:

- **Non-root**: UID/GID 1001, `allowPrivilegeEscalation: false`, all capabilities dropped
- **Resources**: 100m/500m CPU, 128Mi/512Mi memory
- **Readiness**: HTTP GET `/health` on port 8090
- **Volumes**: `kubeconfig` (read-only Secret mount), `workspace` (emptyDir)
- **Auth**: HMAC-SHA256(key, sessionID) → bearer token delivered via Secret → env var

### Cleanup

| Method | Trigger | Description |
|--------|---------|-------------|
| **Explicit delete** | `DELETE /sessions/{id}` | TARSy calls when investigation ends. Deletes pod, Secret, and cache entry |
| **Stale cleanup** | Background ticker | Deletes pods idle > 30 minutes (via `last-activity` annotation) |

If a sandbox pod crashes mid-investigation, shell state (env vars, working directory,
files in emptyDir) is lost. The MCP server detects the transport error, invalidates
the cache, and creates a new pod on the next call. TARSy retains the investigation
context.

## Design Documents

- [Sketch](docs/proposals/cli-mcp-server-sketch.md) — Problem statement and approach
- [Design Overview](docs/proposals/cli-mcp-server-design-overview.md) — Architecture overview
- [Detailed Design](docs/proposals/cli-mcp-server-design.md) — Go types, flows, YAML specs

## Implementation

- [Implementation Plan](docs/implementation/implementation-plan.md) — Phased plan
- [Jira Stories](docs/implementation/stories.md) — Epic SANDBOX-1803 breakdown