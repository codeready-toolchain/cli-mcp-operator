## Context

The CLI MCP Server architecture has two binaries: the MCP server (control plane) and the sandbox agent (data plane). SANDBOX-1806/1807 delivered the agent. SANDBOX-1810 delivered session management (`GetOrCreatePod`, `ExecuteCommand`). SANDBOX-1812 delivered the typed agent HTTP client.

What is missing is the MCP tool surface: a `bash` tool that TARSy can call via the standard MCP `tools/call` interface. The parent design doc (`docs/proposals/cli-mcp-server-design.md`) already specifies the handler shape, input/output types, timeout clamping, and the critical error-model distinction between command failures and infrastructure failures. This change makes that design concrete in `pkg/tools`.

Session identity is **not** a tool parameter. TARSy injects `X-Session-ID` on every HTTP request; the MCP SDK populates `req.Extra.Header` from the transport. The handler reads that header and rejects the call if it is missing.

## Goals / Non-Goals

**Goals:**
- `bash` MCP tool registered via `mcp.AddTool` with typed `BashInput` / `BashOutput`
- Extract `X-Session-ID` from `req.Extra.Header`; reject with a clear error if missing or if `req.Extra` is nil
- Clamp timeout: default 60s when omitted/non-positive; max 300s
- Call `CommandExecutor.ExecuteCommand` (satisfied by `*session.SessionManager`)
- Non-zero exit codes: return `CallToolResult{IsError: true}` **with** `BashOutput` still populated (stdout/stderr/exit_code/duration_ms)
- Infrastructure failures (pod resolve/create, agent unreachable, etc.): return a Go error from the handler (SDK packs as `IsError` tool result **without** structured `BashOutput`)
- Reject empty `command` with a clear validation error
- Unit tests with a mock `CommandExecutor` — no cluster or HTTP required

**Non-Goals:**
- MCP server bootstrap, middleware, HTTP mux, Cobra flags (SANDBOX-1814)
- `DELETE /sessions/{id}`, `/health`, `/metrics` endpoints (SANDBOX-1814)
- Refactoring `SessionManager.ExecuteCommand` to use `AgentClient` (separate cleanup; not required for the tool to work)
- Output truncation, masking, or summarization (TARSy's responsibility per design doc)
- Command allowlisting / read-only enforcement (sandbox is intentionally full bash)
- Warm-pool or session-manager behavior changes

## Decisions

### Decision 1: Place the tool in `pkg/tools/` and follow the mcp-server-devsandbox registration pattern

```go
type BashTool struct {
    executor CommandExecutor
    tool     *mcp.Tool
}

func (t *BashTool) RegisterWith(s *mcp.Server) {
    mcp.AddTool(s, t.tool, t.handle)
}
```

**Rationale:**
- Matches `mcp-server-devsandbox/pkg/tools` (`RegisterWith` + `mcp.AddTool`)
- `mcp.AddTool` derives input/output JSON Schema from Go struct tags automatically
- Keeps tool code out of `pkg/session` and `pkg/server` — session manager stays infrastructure; tools stay MCP-facing
- SANDBOX-1814 will construct `NewBashTool(sessionManager)` and call `RegisterWith(server)`

**Alternative considered:** Inline registration inside `pkg/server`
- Rejected — couples server bootstrap to tool logic and makes unit-testing the handler harder

### Decision 2: Narrow `CommandExecutor` interface instead of depending on `*session.SessionManager`

```go
type CommandExecutor interface {
    ExecuteCommand(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error)
}
```

**Rationale:**
- `*session.SessionManager` already implements this method — no adapter needed at wiring time
- Tests inject a mock without pulling in client-go fakes or HTTP servers
- Matches the implementation plan note: "mock SessionManager interface"
- Avoids an import cycle risk if `pkg/session` ever needed to reference tools (it should not)

**Alternative considered:** Depend directly on `*session.SessionManager`
- Rejected — harder to unit-test; couples tools package to K8s session machinery

### Decision 3: Use typed `mcp.AddTool` — infrastructure failures are tool `IsError`, not protocol errors (option A)

The design-doc pseudocode uses a low-level handler that returns `(*CallToolResult, error)`, where a Go error becomes a **JSON-RPC protocol** error. The modern go-sdk `mcp.AddTool` path treats a regular Go error as a **tool** error (`IsError: true` with error text in content), and only `*jsonrpc.Error` becomes a protocol error.

We deliberately choose **option A**: infrastructure/validation failures return a normal Go error from the typed handler, which the SDK packs as a tool-level `IsError` result (no structured `BashOutput`). We do **not** escalate them to JSON-RPC protocol errors.

**Chosen mapping (AC-aligned):**

| Scenario | Handler return | Client observation |
|---|---|---|
| Exit code 0 | `(nil, BashOutput{...}, nil)` | Success; structured `BashOutput` in content |
| Exit code ≠ 0 | `(&CallToolResult{IsError: true}, BashOutput{...}, nil)` | Tool error **with** structured output (useful for LLM) |
| Missing `X-Session-ID`, empty command, `ExecuteCommand` error | `(nil, BashOutput{}, fmt.Errorf(...))` | Tool error **without** structured `BashOutput` — infrastructure/validation failure |

**Rationale:**
- Preserves the AC distinction: command failures still return stdout/stderr; infrastructure failures do not fabricate command output
- Matches mcp-server-devsandbox conventions (`AddTool` + typed args/results + `fmt.Errorf` → tool `IsError`)
- Same failure shape TARSy already handles for other MCP tools (“tool said it failed”) rather than a harsher protocol-error path
- SDK auto-populates `Content` with JSON text of `BashOutput` when `Content` is unset — satisfies "JSON in `mcp.TextContent`"
- Avoids low-level schema boilerplate

**Alternative considered (option B):** Low-level `Server.AddTool` or return `*jsonrpc.Error` so infrastructure failures are JSON-RPC protocol errors
- **Rejected** — protocol errors deviate from sibling MCP servers and force a different TARSy error path. Option A meets the acceptance criteria without that complexity.

### Decision 4: Session ID from `X-Session-ID` header only — never a tool parameter

```go
if req.Extra == nil || req.Extra.Header.Get("X-Session-ID") == "" {
    return nil, BashOutput{}, fmt.Errorf("missing X-Session-ID header")
}
sessionID := req.Extra.Header.Get("X-Session-ID")
```

**Rationale:**
- Parent design decision Q1: LLM never manages session IDs — eliminates hallucination/typo class of errors
- SDK `RequestExtra.Header` is populated by `StreamableHTTPHandler` from the incoming HTTP request
- Nil-safe check on `req.Extra` covers unit tests and non-HTTP transports

### Decision 5: Timeout clamping — default 60, max 300

```go
timeout := 60
if input.Timeout != nil && *input.Timeout > 0 {
    timeout = *input.Timeout
    if timeout > 300 {
        timeout = 300
    }
}
```

**Rationale:**
- Matches design doc and acceptance criteria exactly
- Omitted, null, zero, or negative timeout → default 60 (pointer omitempty + `> 0` check)
- Values above 300 are clamped, not rejected — avoids LLM retries for "timeout too large"
- Passed through to `ExecuteCommand` / agent as seconds

### Decision 6: Empty command is a validation error

Reject `command == ""` (after trim? **no trim of intentional whitespace-only — treat empty string only**) with `fmt.Errorf("command is required")`.

**Rationale:**
- Schema marks `command` as required, but explicit check gives a clear error if schema validation is bypassed in tests
- Whitespace-only commands are valid bash (no-ops) and are left to the agent — do not over-validate

### Decision 7: Tool annotations — not read-only

```go
Annotations: &mcp.ToolAnnotations{
    // DestructiveHint omitted / false — full bash can mutate workspace state
    ReadOnlyHint: false,
}
```

**Rationale:**
- Sandbox bash is intentionally unrestricted (pipes, redirects, package installs in workspace, etc.)
- Marking `ReadOnlyHint: true` would mislead clients/LLMs
- No allowlist (unlike `vm-ssh` in mcp-server-devsandbox)

### Decision 8: Add go-sdk dependency at the version used by mcp-server-devsandbox

Pin `github.com/modelcontextprotocol/go-sdk` to **v1.4.x** (same major/minor family as mcp-server-devsandbox's current require). Exact patch resolved by `go get` at implementation time.

**Rationale:**
- Tool registration requires the SDK; `go.mod` does not include it yet
- Aligning versions reduces surprise when SANDBOX-1814 also pulls mcp-common middleware that may transitively depend on the SDK

### Decision 9: `BashOutput` field names match `agent.ExecResponse` JSON tags

```go
type BashOutput struct {
    Stdout     string `json:"stdout"`
    Stderr     string `json:"stderr"`
    ExitCode   int    `json:"exit_code"`
    DurationMs int64  `json:"duration_ms"`
}
```

Map 1:1 from `*agent.ExecResponse`. Do not transform or truncate output.

**Rationale:**
- Stable contract for TARSy / LLM parsing
- Design doc: output handling is as-is; masking/summarization is TARSy's job

## Risks / Trade-offs

**Risk:** Design-doc pseudocode showed infrastructure failures as protocol errors; option A surfaces them as tool `IsError` instead.
→ **Mitigation:** Accepted and documented in Decision 3. Clients still see a clear failure without fake command output, consistent with mcp-server-devsandbox.

**Risk:** `req.Extra` may be nil when the tool is invoked outside Streamable HTTP (e.g., future stdio transport or direct unit invocation).
→ **Mitigation:** Explicit nil check; error message identical to missing header case.

**Risk:** Adding go-sdk pulls transitive deps (`jsonschema-go`, etc.) into the module before the server binary exists.
→ **Accepted:** Required for compiling `pkg/tools`. SANDBOX-1814 will use the same dependency.

**Trade-off:** Unit tests in this story vs deferring all tests to SANDBOX-1816.
→ **Accepted:** Follow SANDBOX-1810/1811/1812 pattern — each story ships its own focused unit tests. SANDBOX-1816 remains the broad coverage story.

**Trade-off:** Not refactoring session manager onto `AgentClient` in this story.
→ **Accepted:** Tool only needs `ExecuteCommand`. Refactor is mechanical and can land independently without blocking TARSy tool wiring.

## Open Questions

None — Decision 3 is locked to option A (tool `IsError` for infrastructure/validation failures, not JSON-RPC protocol errors).
