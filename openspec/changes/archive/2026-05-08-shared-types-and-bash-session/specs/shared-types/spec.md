## ADDED Requirements

### Requirement: ExecRequest defines the command execution request format
The `ExecRequest` struct SHALL define the HTTP request body for `POST /exec` with a required `command` field and an optional `timeout` field.

#### Scenario: ExecRequest with command only
- **WHEN** a caller creates an ExecRequest with `Command: "echo hello"`
- **THEN** the struct SHALL serialize to JSON as `{"command":"echo hello"}`

#### Scenario: ExecRequest with command and timeout
- **WHEN** a caller creates an ExecRequest with `Command: "sleep 10"` and `Timeout: 30`
- **THEN** the struct SHALL serialize to JSON as `{"command":"sleep 10","timeout":30}`

#### Scenario: ExecRequest with zero timeout is omitted
- **WHEN** a caller creates an ExecRequest with `Command: "ls"` and `Timeout: 0`
- **THEN** the timeout field SHALL be omitted from JSON serialization (omitempty)

### Requirement: ExecResponse defines the command execution response format
The `ExecResponse` struct SHALL define the HTTP response body for `POST /exec` with stdout, stderr, exit code, and duration fields.

#### Scenario: Successful command response
- **WHEN** a command `echo hello` completes successfully
- **THEN** the ExecResponse SHALL contain `Stdout: "hello\n"`, `Stderr: ""`, `ExitCode: 0`, and a positive `DurationMs` value

#### Scenario: Failed command response
- **WHEN** a command `ls /nonexistent` fails
- **THEN** the ExecResponse SHALL contain a non-empty `Stderr`, `ExitCode` greater than 0, and a positive `DurationMs` value

#### Scenario: ExecResponse JSON field names match wire format
- **WHEN** an ExecResponse is serialized to JSON
- **THEN** the field names SHALL be `stdout`, `stderr`, `exit_code`, and `duration_ms`

### Requirement: AssignRequest defines the token delivery format
The `AssignRequest` struct SHALL define the HTTP request body for `POST /assign` with a required `token` field for HMAC-derived bearer token delivery to warm pool pods.

#### Scenario: AssignRequest serialization
- **WHEN** a caller creates an AssignRequest with `Token: "hmac-derived-token"`
- **THEN** the struct SHALL serialize to JSON as `{"token":"hmac-derived-token"}`

### Requirement: Shared types have no methods or business logic
The shared types SHALL be plain data structs with JSON tags only — no methods, no interfaces, no validation logic. They serve as a wire format, not an abstraction layer.

#### Scenario: Types are importable without side effects
- **WHEN** `pkg/agent` is imported by `pkg/sandbox/handler.go` or `pkg/agent/client.go`
- **THEN** importing the package SHALL not execute any initialization code or register any global state

### Requirement: Shared types live in pkg/agent/types.go
All three shared types (`ExecRequest`, `ExecResponse`, `AssignRequest`) SHALL be defined in `pkg/agent/types.go` so that both the agent HTTP client and the sandbox HTTP handler can import them from a single location.

#### Scenario: Types are importable by agent client
- **WHEN** `pkg/agent/client.go` needs to send an ExecRequest
- **THEN** it SHALL import `ExecRequest` from the same `pkg/agent` package

#### Scenario: Types are importable by sandbox handler
- **WHEN** `pkg/sandbox/handler.go` needs to parse an ExecRequest
- **THEN** it SHALL import `ExecRequest` from `pkg/agent`
