## ADDED Requirements

### Requirement: bash tool is registered with typed input and output schemas

The `bash` MCP tool SHALL be registerable on an `mcp.Server` via `BashTool.RegisterWith`, using `mcp.AddTool` with typed `BashInput` and `BashOutput` so input/output JSON Schemas are derived from Go types.

#### Scenario: Tool name and description
- **WHEN** `NewBashTool` is constructed
- **THEN** the tool name SHALL be `"bash"`
- **AND** the tool description SHALL indicate that it executes a shell command in a persistent sandbox bash session (pipes, redirects, and chaining supported)
- **AND** `ReadOnlyHint` SHALL be false (or unset / not true)

#### Scenario: BashInput schema fields
- **WHEN** the tool's input schema is derived from `BashInput`
- **THEN** it SHALL include required field `command` (string)
- **AND** it SHALL include optional field `timeout` (integer, seconds)
- **AND** it SHALL NOT include a session ID field (session identity is header-only)

#### Scenario: BashOutput schema fields
- **WHEN** the tool returns structured output
- **THEN** `BashOutput` SHALL serialize with JSON fields `stdout`, `stderr`, `exit_code`, and `duration_ms`

#### Scenario: RegisterWith adds the tool to the server
- **WHEN** `RegisterWith` is called with an `mcp.Server`
- **THEN** the server SHALL expose a tool named `"bash"` that dispatches to the bash handler

### Requirement: Session ID is extracted from the X-Session-ID HTTP header

The handler SHALL read the session ID exclusively from `req.Extra.Header.Get("X-Session-ID")` and SHALL reject requests that lack it.

#### Scenario: Successful header extraction
- **WHEN** a tools/call request includes `X-Session-ID: inv-abc123` in `req.Extra.Header`
- **THEN** the handler SHALL use `inv-abc123` as the session ID passed to `ExecuteCommand`

#### Scenario: Missing X-Session-ID header
- **WHEN** `req.Extra` is nil
- **OR** `req.Extra.Header.Get("X-Session-ID")` is empty
- **THEN** the handler SHALL return an error whose message includes `missing X-Session-ID header`
- **AND** it SHALL NOT call `ExecuteCommand`

#### Scenario: Session ID is not a tool parameter
- **WHEN** the LLM supplies tool arguments
- **THEN** there SHALL be no `session_id` (or equivalent) field in `BashInput`
- **AND** any session-like value in arguments SHALL be ignored for routing purposes

### Requirement: Timeout is clamped to default 60s and maximum 300s

The handler SHALL normalize the optional `timeout` argument before calling `ExecuteCommand`.

#### Scenario: Default timeout when omitted
- **WHEN** `timeout` is omitted (nil)
- **THEN** `ExecuteCommand` SHALL be called with `timeoutSec == 60`

#### Scenario: Default timeout when non-positive
- **WHEN** `timeout` is present and less than or equal to 0
- **THEN** `ExecuteCommand` SHALL be called with `timeoutSec == 60`

#### Scenario: Timeout within range is passed through
- **WHEN** `timeout` is present and in the range 1..300 inclusive
- **THEN** `ExecuteCommand` SHALL be called with that exact value

#### Scenario: Timeout above maximum is clamped
- **WHEN** `timeout` is present and greater than 300
- **THEN** `ExecuteCommand` SHALL be called with `timeoutSec == 300`
- **AND** the handler SHALL NOT return an error solely because the requested timeout exceeded the max

### Requirement: Successful command execution returns structured BashOutput

On successful agent execution (including non-zero exit codes), the handler SHALL map `*agent.ExecResponse` to `BashOutput` without truncating or transforming stdout/stderr.

#### Scenario: Zero exit code is a successful tool result
- **WHEN** `ExecuteCommand` returns an `ExecResponse` with `ExitCode == 0`
- **THEN** the handler SHALL return no error
- **AND** the result SHALL include `BashOutput` with matching `stdout`, `stderr`, `exit_code`, and `duration_ms`
- **AND** `CallToolResult.IsError` SHALL be false (or unset)

#### Scenario: Non-zero exit code sets IsError but still returns output
- **WHEN** `ExecuteCommand` returns an `ExecResponse` with `ExitCode != 0`
- **THEN** the handler SHALL return no Go error
- **AND** `CallToolResult.IsError` SHALL be true
- **AND** the structured `BashOutput` SHALL still contain the command's `stdout`, `stderr`, `exit_code`, and `duration_ms`
- **AND** the text content SHALL include the JSON serialization of that `BashOutput` (via SDK population of `Content` when unset)

#### Scenario: Output is returned as-is
- **WHEN** the agent returns stdout/stderr of any length within agent client limits
- **THEN** the handler SHALL NOT truncate, mask, or rewrite those fields

### Requirement: Infrastructure and validation failures return handler errors without BashOutput

Only infrastructure and validation failures SHALL cause the handler to return a Go error. Those failures MUST NOT be presented as a successful command result with fabricated exit codes.

#### Scenario: ExecuteCommand failure is an infrastructure error
- **WHEN** `ExecuteCommand` returns an error (pod creation failed, agent unreachable, etc.)
- **THEN** the handler SHALL return a Go error wrapping or including that failure (message SHOULD include context such as `sandbox exec failed`)
- **AND** it SHALL NOT return a populated success `BashOutput` for that call

#### Scenario: Empty command is rejected
- **WHEN** `command` is the empty string
- **THEN** the handler SHALL return an error indicating the command is required
- **AND** it SHALL NOT call `ExecuteCommand`

#### Scenario: Command failure vs infrastructure failure are distinguishable
- **WHEN** a command exits non-zero
- **THEN** the client receives structured `BashOutput` with `IsError: true`
- **WHEN** infrastructure fails
- **THEN** the client receives a tool error without structured `BashOutput` command fields as a successful mapping of agent output

### Requirement: Handler depends on CommandExecutor for testability

The bash tool SHALL accept a `CommandExecutor` interface rather than a concrete `*session.SessionManager`, so unit tests can mock execution.

#### Scenario: SessionManager satisfies CommandExecutor
- **WHEN** the server is wired in SANDBOX-1814
- **THEN** `*session.SessionManager` SHALL be usable as the `CommandExecutor` without an adapter (method signature compatible)

#### Scenario: Unit tests use a mock executor
- **WHEN** `pkg/tools/bash_test.go` runs
- **THEN** tests SHALL exercise the handler with a mock `CommandExecutor`
- **AND** tests SHALL NOT require a real Kubernetes cluster or live agent HTTP server

### Requirement: go-sdk dependency is available for compilation

The module SHALL require `github.com/modelcontextprotocol/go-sdk` so `pkg/tools` compiles.

#### Scenario: Module builds with tools package
- **WHEN** `go test ./pkg/tools/...` is run after implementation
- **THEN** the package SHALL compile and tests SHALL pass
- **AND** the go-sdk version SHALL be in the v1.4.x family aligned with mcp-server-devsandbox
