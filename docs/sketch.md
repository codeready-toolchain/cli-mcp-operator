# CLI MCP Server — Sketch

**Status:** Implemented (as-built)

**Related:** [Architecture Overview](architecture-overview.md) · [Detailed Design](design.md)

## Problem

TARSy investigation agents are limited to pre-built, structured MCP tools. When an investigation needs a command outside that set — pipes through `jq`, cross-cluster diffs, multi-step shell workflows — the LLM cannot proceed without a new tool, rebuild, and redeploy.

Structured tools also cannot:

- Pipe or chain commands
- Keep cwd, env vars, and intermediate files across calls
- Use ordinary Unix tooling on command output

Local coding agents solve this with a persistent shell. The goal is the same capability for TARSy, inside an isolated Kubernetes sandbox.

## Goal

An MCP server that gives agents a **sandboxed bash environment**: one pod per investigation session, a persistent shell, and CLIs from the sandbox image. Security comes from the sandbox boundary (RBAC, pod isolation, network policy, agent auth) — not from command allowlists.

This **complements** existing structured MCP servers. Structured tools stay preferred for well-known, high-frequency operations; this server covers the long tail.

**Initial CLIs:** `oc`, `kubectl`, plus utilities such as `jq`, `yq`, `curl`. More CLIs mean image changes only — no server code changes.

## Approach

```
Client → bash(command) + X-Session-ID → cli-mcp-server → sandbox pod (agent + bash + CLIs) → target APIs
```

- **Control plane:** `cli-mcp-server` — MCP `bash` tool, pod lifecycle, command proxy
- **Data plane:** `sandbox-agent` inside each pod — persistent bash over HTTP
- **Session routing:** `X-Session-ID` header (client/orchestrator), not a tool parameter
- **State:** lives in the pod (bash process + `/workspace`); the MCP server is stateless and multi-replica ready

Optional warm pool pre-creates unassigned pods to avoid cold start.

## Why not extend structured servers?

| | Structured MCP servers | cli-mcp-server |
|---|---|---|
| Tool model | One tool per use case | Single `bash` tool |
| LLM control | Parameters only | Full shell |
| Adding capability | New Go tool + deploy | Install binary in image |
| Session | Stateless | Per-session sandbox pod |

## Out of scope

- Replacing existing structured MCP servers
- Write/mutate cluster operations (read-only via investigation RBAC)
- Interactive stdin (`oc edit`, `oc exec -it`)
- Per-user credentials (shared investigation kubeconfig)
