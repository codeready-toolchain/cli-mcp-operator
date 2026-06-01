## ADDED Requirements

### Requirement: Agent reads initial state from environment
The agent binary SHALL read `SANDBOX_AUTH_TOKEN` from the environment to determine its initial state.

#### Scenario: Token present starts agent in assigned state
- **WHEN** `SANDBOX_AUTH_TOKEN` environment variable is set to a non-empty value
- **THEN** the agent SHALL start with `AgentState` in assigned mode and log "starting in assigned state"

#### Scenario: Token absent starts agent in unassigned state
- **WHEN** `SANDBOX_AUTH_TOKEN` environment variable is empty or not set
- **THEN** the agent SHALL start with `AgentState` in unassigned mode and log "starting in unassigned state (warm pool)"

### Requirement: Agent creates BashSession eagerly
The agent SHALL create a `BashSession` at startup using `NewBashSession(NewDefaultBashConfig())` and exit fatally if bash fails to start.

#### Scenario: Bash starts successfully
- **WHEN** the agent starts and bash is available in PATH
- **THEN** a BashSession SHALL be created and the agent SHALL proceed to start the HTTP server

#### Scenario: Bash unavailable causes fatal exit
- **WHEN** the agent starts but bash is not available
- **THEN** the agent SHALL log the error and exit with a non-zero status code

### Requirement: Agent starts HTTP server on port 8090
The agent SHALL start an `http.Server` listening on `:8090` with routes registered using Go 1.22+ method-based routing.

#### Scenario: Server starts and listens
- **WHEN** the agent starts successfully
- **THEN** an HTTP server SHALL listen on `:8090` and log "listening on :8090"

#### Scenario: Routes are registered with method patterns
- **WHEN** the HTTP mux is configured
- **THEN** it SHALL register `"POST /exec"`, `"POST /assign"`, and `"GET /health"` using Go 1.22+ method-based patterns

### Requirement: Agent shuts down gracefully on SIGTERM
The agent SHALL handle `SIGTERM` and `SIGINT` signals, drain in-flight HTTP requests via `http.Server.Shutdown()`, and close the BashSession before exiting.

#### Scenario: SIGTERM triggers graceful shutdown
- **WHEN** the agent receives SIGTERM
- **THEN** it SHALL call `srv.Shutdown()` with a 310-second timeout context, then call `session.Close()`, then exit 0

#### Scenario: In-flight exec request completes during shutdown
- **WHEN** SIGTERM is received while a POST /exec request is in progress
- **THEN** the server SHALL wait for the in-flight request to complete (up to 310 seconds) before closing

#### Scenario: Shutdown timeout exceeded
- **WHEN** `srv.Shutdown()` exceeds the 310-second timeout
- **THEN** the agent SHALL log the shutdown error, close the BashSession, and exit

#### Scenario: SIGINT also triggers shutdown
- **WHEN** the agent receives SIGINT
- **THEN** it SHALL follow the same graceful shutdown sequence as SIGTERM

### Requirement: Shutdown timeout accommodates maximum command duration
The shutdown timeout SHALL be 310 seconds (`MaxTimeout` 300s + 10s buffer) to ensure the longest possible in-flight command completes.

#### Scenario: Kubernetes terminationGracePeriodSeconds coordination
- **WHEN** the agent pod spec is configured
- **THEN** `terminationGracePeriodSeconds` SHALL be set to 310 to match the agent's shutdown timeout

### Requirement: Agent logs version at startup
The agent SHALL print version information to stderr at startup.

#### Scenario: Version logged on start
- **WHEN** the agent starts
- **THEN** it SHALL print `sandbox-agent <commit> (built <time>)` to stderr using the version package
