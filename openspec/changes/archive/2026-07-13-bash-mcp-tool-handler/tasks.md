## 1. Package skeleton and types

- [ ] 1.1 Create `pkg/tools/bash.go` with package `tools`
- [ ] 1.2 Define `BashInput` struct: `Command string` (`json:"command"`, jsonschema required + description), `Timeout *int` (`json:"timeout,omitempty"`, jsonschema description default 60 max 300)
- [ ] 1.3 Define `BashOutput` struct: `Stdout`, `Stderr` string; `ExitCode` int (`json:"exit_code"`); `DurationMs` int64 (`json:"duration_ms"`)
- [ ] 1.4 Define `CommandExecutor` interface with `ExecuteCommand(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error)`
- [ ] 1.5 Define `BashTool` struct holding `executor CommandExecutor` and `tool *mcp.Tool`

## 2. Constructor and registration

- [ ] 2.1 Implement `NewBashTool(executor CommandExecutor) *BashTool` — panics or returns clearly if executor is nil is optional; prefer simple assignment (caller responsibility)
- [ ] 2.2 Construct `mcp.Tool` with `Name: "bash"`, descriptive `Description`, and `Annotations` with `ReadOnlyHint: false`
- [ ] 2.3 Implement `RegisterWith(s *mcp.Server)` calling `mcp.AddTool(s, t.tool, t.handle)`
- [ ] 2.4 Add `github.com/modelcontextprotocol/go-sdk` v1.4.x to `go.mod` via `go get`

## 3. Handler — validation and session routing

- [ ] 3.1 Implement `handle(ctx context.Context, req *mcp.CallToolRequest, input BashInput) (*mcp.CallToolResult, BashOutput, error)`
- [ ] 3.2 If `req.Extra == nil` or `X-Session-ID` header is empty, return error `"missing X-Session-ID header"`
- [ ] 3.3 If `input.Command == ""`, return error indicating command is required
- [ ] 3.4 Clamp timeout: default 60 if nil or `<= 0`; otherwise `min(timeout, 300)`

## 4. Handler — execution and result mapping

- [ ] 4.1 Call `executor.ExecuteCommand(ctx, sessionID, input.Command, timeout)`
- [ ] 4.2 On executor error, return `fmt.Errorf("sandbox exec failed: %w", err)` (no BashOutput payload)
- [ ] 4.3 On success, map `ExecResponse` → `BashOutput` field-for-field
- [ ] 4.4 If `ExitCode == 0`, return `(nil, output, nil)`
- [ ] 4.5 If `ExitCode != 0`, return `(&mcp.CallToolResult{IsError: true}, output, nil)`

## 5. Unit tests

- [ ] 5.1 Create `pkg/tools/bash_test.go` with a mock `CommandExecutor` (func field or testify mock)
- [ ] 5.2 Test: missing `X-Session-ID` (nil Extra and empty header) → error, executor not called
- [ ] 5.3 Test: empty command → error, executor not called
- [ ] 5.4 Test: omitted timeout → executor called with 60
- [ ] 5.5 Test: timeout 0 / negative → executor called with 60
- [ ] 5.6 Test: timeout 120 → executor called with 120
- [ ] 5.7 Test: timeout 500 → executor called with 300
- [ ] 5.8 Test: successful exit 0 → `IsError` false/nil result, BashOutput fields match
- [ ] 5.9 Test: exit code 1 → `CallToolResult.IsError == true`, BashOutput still populated, no Go error
- [ ] 5.10 Test: executor returns error → Go error contains `sandbox exec failed`, no success output mapping
- [ ] 5.11 Test: session ID from header is forwarded to executor unchanged
- [ ] 5.12 Test: `RegisterWith` registers a tool named `bash` on a new `mcp.Server` (list tools or equivalent)
- [ ] 5.13 Run `go test ./pkg/tools/...` and ensure pass

## 6. Verification against acceptance criteria

- [ ] 6.1 Confirm AC: extracts `X-Session-ID`; rejects if missing
- [ ] 6.2 Confirm AC: timeout default 60, max 300
- [ ] 6.3 Confirm AC: non-zero exit → `IsError: true` with output retained
- [ ] 6.4 Confirm AC: only infrastructure/validation failures return handler errors
- [ ] 6.5 Confirm AC: structured `BashOutput` JSON available via SDK text/structured content
