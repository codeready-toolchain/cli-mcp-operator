## ADDED Requirements

### Requirement: AgentState tracks unassigned/assigned lifecycle
The `AgentState` struct SHALL manage the agent's lifecycle state using a `sync.Mutex` protecting an `assigned` flag and a `token` string.

#### Scenario: New agent with token starts assigned
- **WHEN** `NewAgentState` is called with a non-empty token string
- **THEN** `IsAssigned()` SHALL return true and `GetToken()` SHALL return the provided token

#### Scenario: New agent without token starts unassigned
- **WHEN** `NewAgentState` is called with an empty string
- **THEN** `IsAssigned()` SHALL return false and `GetToken()` SHALL return an empty string

#### Scenario: Assign transitions from unassigned to assigned
- **WHEN** `Assign` is called on an unassigned AgentState with a non-empty token
- **THEN** `IsAssigned()` SHALL return true and `GetToken()` SHALL return the provided token

#### Scenario: Assign rejects if already assigned
- **WHEN** `Assign` is called on an already-assigned AgentState
- **THEN** it SHALL return a non-nil error and the existing token SHALL not be changed

#### Scenario: AgentState methods are thread-safe
- **WHEN** `IsAssigned`, `GetToken`, and `Assign` are called concurrently from multiple goroutines
- **THEN** all operations SHALL be serialized via the mutex without data races

### Requirement: Handler groups shared dependencies
The `Handler` struct SHALL hold a `*BashSession` and an `*AgentState`, providing method handlers for all three HTTP endpoints.

#### Scenario: NewHandler creates a valid handler
- **WHEN** `NewHandler` is called with a valid BashSession and AgentState
- **THEN** it SHALL return a Handler with both dependencies accessible to all endpoint methods

### Requirement: POST /exec executes commands with bearer token auth
The `HandleExec` method SHALL authenticate requests using a bearer token compared with `hmac.Equal()`, parse the `ExecRequest`, execute the command via `BashSession.Execute()`, and return an `ExecResponse`.

#### Scenario: Successful command execution
- **WHEN** a POST /exec request has a valid bearer token and a well-formed ExecRequest with `command: "echo hello"`
- **THEN** the handler SHALL return 200 with a JSON ExecResponse containing stdout, stderr, exit_code, and duration_ms

#### Scenario: Agent not assigned rejects exec
- **WHEN** a POST /exec request is received while the agent is in unassigned state
- **THEN** the handler SHALL return 503 with `{"error": "agent not assigned"}`

#### Scenario: Missing authorization header returns 401
- **WHEN** a POST /exec request has no Authorization header
- **THEN** the handler SHALL return 401 with `{"error": "missing or invalid authorization header"}`

#### Scenario: Malformed authorization header returns 401
- **WHEN** a POST /exec request has an Authorization header without the `Bearer ` prefix
- **THEN** the handler SHALL return 401 with `{"error": "missing or invalid authorization header"}`

#### Scenario: Wrong bearer token returns 401
- **WHEN** a POST /exec request has a bearer token that does not match the stored token
- **THEN** the handler SHALL return 401 with `{"error": "unauthorized"}`

#### Scenario: Token comparison is constant-time
- **WHEN** the bearer token is compared against the stored token
- **THEN** the comparison SHALL use `hmac.Equal()` to prevent timing side-channel attacks

#### Scenario: Malformed JSON body returns 400
- **WHEN** a POST /exec request has an invalid JSON body
- **THEN** the handler SHALL return 400 with `{"error": "invalid request body"}`

#### Scenario: Empty command returns 400
- **WHEN** a POST /exec request has an ExecRequest with an empty command field
- **THEN** the handler SHALL return 400 with `{"error": "command is required"}`

#### Scenario: Non-zero exit code is a valid result
- **WHEN** a command exits with code 1 (e.g., `ls /nonexistent`)
- **THEN** the handler SHALL return 200 with an ExecResponse containing `exit_code: 1` (not an HTTP error)

#### Scenario: BashSession infrastructure error returns 500
- **WHEN** `BashSession.Execute()` returns a non-nil error (e.g., bash crash, stdin write failure)
- **THEN** the handler SHALL return 500 with `{"error": "<error message>"}`

#### Scenario: Timeout conversion passes raw value to BashSession
- **WHEN** the ExecRequest has a `timeout` field (in seconds)
- **THEN** the handler SHALL convert it to `time.Duration` and pass it to `BashSession.Execute()` without clamping (BashSession handles clamping internally)

#### Scenario: ExecResult is mapped to ExecResponse
- **WHEN** `BashSession.Execute()` returns an ExecResult
- **THEN** the handler SHALL map it to an `agent.ExecResponse` with `DurationMs: result.Duration.Milliseconds()`

### Requirement: POST /assign delivers tokens to warm-pool pods
The `HandleAssign` method SHALL parse an `AssignRequest`, validate the token, and transition the agent from unassigned to assigned state.

#### Scenario: Successful token assignment
- **WHEN** a POST /assign request has a valid AssignRequest with a non-empty token and the agent is unassigned
- **THEN** the handler SHALL transition to assigned state and return 200 with an empty body

#### Scenario: Already assigned returns 409
- **WHEN** a POST /assign request is received while the agent is already assigned
- **THEN** the handler SHALL return 409 with `{"error": "already assigned"}`

#### Scenario: Empty token returns 400
- **WHEN** a POST /assign request has an AssignRequest with an empty token field
- **THEN** the handler SHALL return 400 with `{"error": "token is required"}`

#### Scenario: Malformed JSON body returns 400
- **WHEN** a POST /assign request has an invalid JSON body
- **THEN** the handler SHALL return 400 with `{"error": "invalid request body"}`

#### Scenario: Assign is unauthenticated
- **WHEN** a POST /assign request is received
- **THEN** the handler SHALL NOT require an Authorization header (the warm-pool pod has no token yet)

### Requirement: GET /health reports bash session liveness
The `HandleHealth` method SHALL return the bash session's alive/dead status for use as a Kubernetes readiness probe.

#### Scenario: Healthy agent returns 200
- **WHEN** `BashSession.IsAlive()` returns true
- **THEN** the handler SHALL return 200 with `{"status": "ok"}`

#### Scenario: Unhealthy agent returns 503
- **WHEN** `BashSession.IsAlive()` returns false (bash crashed and not yet respawned)
- **THEN** the handler SHALL return 503 with `{"status": "unhealthy"}`

#### Scenario: Health check is unauthenticated
- **WHEN** a GET /health request is received
- **THEN** the handler SHALL NOT require an Authorization header (kubelet calls it directly)

### Requirement: Error responses use consistent JSON format
All error responses SHALL use a JSON object with an `error` field containing a human-readable message.

#### Scenario: Error response format
- **WHEN** any handler returns an error response
- **THEN** the response body SHALL be `{"error": "<message>"}` with Content-Type `application/json`

### Requirement: HTTP routing uses Go 1.22+ method-based patterns
The HTTP mux SHALL use Go 1.22+ method-based routing patterns (`"POST /exec"`, `"POST /assign"`, `"GET /health"`) for automatic method enforcement.

#### Scenario: Wrong HTTP method returns 405
- **WHEN** a GET request is sent to /exec
- **THEN** the ServeMux SHALL return 405 Method Not Allowed automatically
