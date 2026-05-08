## Context

The CLI MCP Server architecture has two binaries: the MCP server (control plane, manages pod lifecycle) and the sandbox agent (data plane, runs inside each sandbox pod). They communicate via HTTP — the server sends commands to the agent, the agent executes them in a persistent bash session and returns structured results.

SANDBOX-1806 delivers the two foundational pieces: the shared HTTP contract types and the persistent bash session. These sit at the bottom of the call stack — every command execution flows through them.

## Goals / Non-Goals

**Goals:**
- Define a clean, simple HTTP contract between server and agent (`ExecRequest`, `ExecResponse`, `AssignRequest`)
- Implement a persistent bash session that preserves state between calls (env vars, working directory, files)
- Handle timeouts by killing the command without killing bash
- Recover from bash crashes gracefully (respawn on next call)
- Keep `pkg/sandbox` self-contained with no dependency on `pkg/agent` wire format
- Return output as-is — no truncation, no transformation (TARSy handles compression)

**Non-Goals:**
- HTTP handler implementation (SANDBOX-1807)
- Agent entry point and HTTP server setup (SANDBOX-1807)
- Authentication/authorization (SANDBOX-1807)
- Container image (SANDBOX-1808)
- Unit tests (SANDBOX-1809)
- Output truncation or transformation (design principle: "dumb proxy")

## Decisions

### Decision 1: Pipe-based bash session, not PTY
Use `os/exec` with `StdinPipe()`, `StdoutPipe()`, `StderrPipe()` instead of a pseudo-terminal.

**Rationale:**
- Clean stdout/stderr separation — important for structured JSON responses
- No terminal control sequences that have no value for LLM consumption
- Pure Go stdlib, no external dependencies
- Well-known pattern used by OpenHands, Claude Code

**Alternative considered:** PTY-based session
- Rejected because PTY merges stdout/stderr, adds ANSI escape sequences, and requires external dependencies — complexity with no benefit for LLM consumption

### Decision 2: Concurrent pipe readers with shared buffers
Use two goroutines writing to `strings.Builder` buffers, synchronized with `sync.WaitGroup`.

**Rationale:**
- Avoids pipe buffer deadlocks that occur with sequential reads (a command writing heavily to stderr blocks while we wait for stdout)
- Simpler than channel-based approach — just accumulate into buffers
- No streaming needed since we return the full response at the end

**Alternative considered:** Sequential read (stdout first, then stderr)
- Rejected due to pipe buffer deadlock risk

**Alternative considered:** Channel-based concurrent readers
- Rejected — adds ~15-20 lines of complexity (custom result types, channel coordination) for zero functional benefit

### Decision 3: SIGKILL to process group with 5-second hard deadline
On timeout, send SIGKILL to the command's process group. Wait up to 5 seconds for exit. If process won't die, mark session dead and trigger crash recovery on next call.

**Rationale:**
- Process group isolation (`Setpgid: true`) means SIGKILL kills the command but not bash
- 5-second wait is a reasonable trade-off — timeouts are already exceptional
- Reuses crash recovery path for the "process won't die" edge case
- Clean exit is more important than saving 5 seconds

**Alternative considered:** Fire-and-forget SIGKILL
- Rejected — risks orphaned processes and corrupted pipe state

**Alternative considered:** Async cleanup goroutine
- Rejected — adds ~30-40 lines of concurrency code (generation counter, cleaning flag) for race conditions that are hard to get right

### Decision 4: Crash recovery deferred to next call
When bash dies (pipe EOF), fail the current Execute() and respawn on the next call. Prepend "[session state was reset]" to stderr.

**Rationale:**
- Simple — no retry logic, clear failure semantics
- Avoids retrying a potentially crash-causing command (e.g., OOM)
- Matches the parent design docs description
- Extra round-trip cost is negligible (~0.01% crash frequency, LLM handles retry naturally)

**Alternative considered:** Immediate respawn and retry
- Rejected — risks crash-looping on OOM-causing commands

### Decision 5: Domain-specific ExecResult type
BashSession returns `ExecResult` (in `pkg/sandbox`), not `agent.ExecResponse`.

**Rationale:**
- Keeps `pkg/sandbox` self-contained with no import of `pkg/agent`
- Uses idiomatic `time.Duration` instead of `int64` milliseconds
- Handler does the mapping (~5 lines): `DurationMs: result.Duration.Milliseconds()`

**Note:** The parent design has Execute() returning `*ExecResponse` directly. This is a deliberate refinement for cleaner package separation.

### Decision 6: Config struct constructor
`NewBashSession(cfg BashConfig)` instead of bare `NewBashSession()`.

**Rationale:**
- Stable constructor signature — adding settings later doesn't break callers
- Consistent with `SandboxConfig` pattern in the parent design
- Config is currently empty but provides a clear extension point

**Note:** The parent design has `NewBashSession()` with no parameters. This refinement is functionally equivalent since `BashConfig` is currently empty.

### Decision 7: Eager initialization
Bash process starts in the constructor, not on first Execute().

**Rationale:**
- Fail-fast — if bash isn't available, the agent knows at startup
- Zero first-command latency
- Simpler health checks (alive = bash running, dead = not running)
- Idle bash costs <1MB RSS — negligible even for warm pool pods

### Decision 8: No output truncation
Output is returned as-is with no size limits.

**Rationale:**
- Parent design explicitly states: "The MCP server returns command output as-is — no truncation, no transformation"
- Design principle: "dumb proxy" — output treatment is TARSy's responsibility
- Truncation risks losing information needed for accurate TARSy summarization
- OOM risk mitigated by command timeouts (60-300s) and pod restart via `RestartPolicy: OnFailure`

## Risks / Trade-offs

**Risk:** Commands producing very large output could OOM the sandbox agent
→ **Mitigation:** Command timeouts (default 60s, max 300s) limit duration. Pod `RestartPolicy: OnFailure` restarts the agent. Crash recovery handles it on the next call. TARSy's summarization compresses the response downstream.

**Risk:** UUID-based delimiter could theoretically collide with command output
→ **Mitigation:** 8-character UUID suffix gives ~1/4 billion collision probability per command. Practically impossible.

**Risk:** Concurrent health check reads `dead` flag while Execute() modifies it
→ **Mitigation:** `IsAlive()` acquires the mutex before reading `dead`.

**Trade-off:** ExecResult adds a type + mapping code vs. using ExecResponse directly
→ **Accepted:** Clean package separation is worth ~5 lines of mapping in the handler.

**Trade-off:** BashConfig is currently empty
→ **Accepted:** Provides a stable extension point. Better to have it from the start than to change the constructor signature later.

## Open Questions

None — all design decisions are resolved.
