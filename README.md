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

## Design Documents

- [Sketch](docs/proposals/cli-mcp-server-sketch.md) — Problem statement and approach
- [Design Overview](docs/proposals/cli-mcp-server-design-overview.md) — Architecture overview
- [Detailed Design](docs/proposals/cli-mcp-server-design.md) — Go types, flows, YAML specs

## Implementation

- [Implementation Plan](docs/implementation/implementation-plan.md) — Phased plan
- [Jira Stories](docs/implementation/stories.md) — Epic SANDBOX-1803 breakdown