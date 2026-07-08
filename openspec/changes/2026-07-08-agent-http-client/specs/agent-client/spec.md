## ADDED Requirements

### Requirement: AgentClient provides typed HTTP methods for the sandbox agent API

The `AgentClient` SHALL provide `Execute`, `Assign`, and `HealthCheck` methods that communicate with the sandbox agent over HTTP, returning typed errors for all failure modes.

#### Scenario: Successful command execution via Execute
- **WHEN** `Execute` is called with a valid pod IP, bearer token, and `ExecRequest`
- **AND** the agent returns HTTP 200 with a valid JSON `ExecResponse`
- **THEN** it SHALL return the decoded `*ExecResponse` with no error
- **AND** the HTTP request SHALL be `POST http://<podIP>:<port>/exec`
- **AND** the `Authorization` header SHALL be `Bearer <token>`
- **AND** the `Content-Type` header SHALL be `application/json`

#### Scenario: Execute returns NetworkError on transport failure
- **WHEN** `Execute` is called
- **AND** the HTTP request fails with a transport-level error (connection refused, timeout, DNS resolution failure, or network unreachable)
- **THEN** it SHALL return a `*NetworkError` with `Op` set to `"execute"`
- **AND** the `URL` field SHALL contain the target URL
- **AND** the `Err` field SHALL wrap the underlying `net` error
- **AND** `errors.Is(err, context.DeadlineExceeded)` SHALL return true when the failure was a context deadline

#### Scenario: Execute returns StatusError on non-200 response
- **WHEN** `Execute` is called
- **AND** the agent returns a non-200 HTTP status code
- **THEN** it SHALL return a `*StatusError` with the `StatusCode` field set to the response status
- **AND** the `Body` field SHALL contain at most 512 bytes of the response body
- **AND** the `Op` field SHALL be `"execute"`

#### Scenario: Execute returns DecodeError on malformed JSON
- **WHEN** `Execute` is called
- **AND** the agent returns HTTP 200 with a body that is not valid JSON
- **THEN** it SHALL return a `*DecodeError` with the `Err` field wrapping the JSON decode error
- **AND** the `Body` field SHALL contain at most 512 bytes of the raw response body
- **AND** the `StatusCode` field SHALL be 200
- **AND** the `Op` field SHALL be `"execute"`

#### Scenario: Execute returns DecodeError when response exceeds maximum size
- **WHEN** `Execute` is called
- **AND** the agent returns HTTP 200 with a body larger than the configured maximum response size (default 10 MB)
- **THEN** it SHALL return a `*DecodeError`
- **AND** the error message SHALL indicate the response exceeded the size limit
- **AND** the `StatusCode` field SHALL be 200

#### Scenario: Execute respects context cancellation
- **WHEN** `Execute` is called with a context that is cancelled before the HTTP response arrives
- **THEN** it SHALL return a `*NetworkError` wrapping `context.Canceled`

#### Scenario: Execute respects context deadline
- **WHEN** `Execute` is called with a context whose deadline expires before the HTTP response arrives
- **THEN** it SHALL return a `*NetworkError` wrapping `context.DeadlineExceeded`

### Requirement: Assign delivers a token to a warm-pool agent

The `Assign` method SHALL POST a token to the agent's `/assign` endpoint for warm-pool pod activation.

#### Scenario: Successful token assignment
- **WHEN** `Assign` is called with a valid pod IP and `AssignRequest`
- **AND** the agent returns HTTP 200
- **THEN** it SHALL return nil error
- **AND** the HTTP request SHALL be `POST http://<podIP>:<port>/assign`
- **AND** the `Content-Type` header SHALL be `application/json`
- **AND** no `Authorization` header SHALL be sent (the `/assign` endpoint is unauthenticated)

#### Scenario: Assign returns StatusError on 409 Conflict (already assigned)
- **WHEN** `Assign` is called
- **AND** the agent returns HTTP 409
- **THEN** it SHALL return a `*StatusError` with `StatusCode` 409
- **AND** callers can detect this case via `errors.As` and check `StatusCode == 409`

#### Scenario: Assign returns NetworkError on transport failure
- **WHEN** `Assign` is called
- **AND** the HTTP request fails with a transport-level error
- **THEN** it SHALL return a `*NetworkError` with `Op` set to `"assign"`
- **AND** the `Err` field SHALL wrap the underlying error

#### Scenario: Assign returns StatusError on non-200/non-409 response
- **WHEN** `Assign` is called
- **AND** the agent returns an unexpected HTTP status (e.g., 500, 503)
- **THEN** it SHALL return a `*StatusError` with the `StatusCode` field set to the response status

### Requirement: HealthCheck verifies agent readiness

The `HealthCheck` method SHALL issue a GET to `/health` with no authentication.

#### Scenario: Agent is healthy
- **WHEN** `HealthCheck` is called with a valid pod IP
- **AND** the agent returns HTTP 200
- **THEN** it SHALL return nil error

#### Scenario: Agent is unhealthy (non-200 response)
- **WHEN** `HealthCheck` is called
- **AND** the agent returns a non-200 HTTP status (e.g., 503 when bash process is dead)
- **THEN** it SHALL return a `*StatusError` with the `StatusCode` field set to the response status
- **AND** the `Op` field SHALL be `"health_check"`

#### Scenario: Agent is unreachable
- **WHEN** `HealthCheck` is called
- **AND** the HTTP request fails with a transport-level error (connection refused, timeout)
- **THEN** it SHALL return a `*NetworkError` with `Op` set to `"health_check"`

### Requirement: AgentClient supports configurable timeouts

The `AgentClient` SHALL support configurable timeouts that apply to all HTTP requests.

#### Scenario: Default timeout is applied
- **WHEN** `NewAgentClient()` is called with no options
- **THEN** the internal `http.Client.Timeout` SHALL be 30 seconds

#### Scenario: Custom timeout via WithTimeout option
- **WHEN** `NewAgentClient(WithTimeout(10 * time.Second))` is called
- **THEN** the internal `http.Client.Timeout` SHALL be 10 seconds
- **AND** requests that exceed 10 seconds SHALL return a `*NetworkError`

#### Scenario: Per-request timeout via context overrides client timeout
- **WHEN** `Execute` is called with a context deadline shorter than the client timeout
- **THEN** the request SHALL be cancelled when the context deadline expires
- **AND** the shorter deadline SHALL take precedence

#### Scenario: Default agent port
- **WHEN** `NewAgentClient()` is called with no options
- **THEN** the agent port SHALL default to 8090

#### Scenario: Custom port via WithPort option
- **WHEN** `NewAgentClient(WithPort(9090))` is called
- **THEN** all requests SHALL target port 9090

#### Scenario: Default maximum response size
- **WHEN** `NewAgentClient()` is called with no options
- **THEN** the maximum response body size for successful responses SHALL be 10 MB (10,485,760 bytes)

#### Scenario: Custom maximum response size via WithMaxResponseSize option
- **WHEN** `NewAgentClient(WithMaxResponseSize(5 * 1024 * 1024))` is called
- **THEN** successful response bodies exceeding 5 MB SHALL return a `*DecodeError`

### Requirement: Typed errors support errors.Is and errors.As

All error types (`NetworkError`, `StatusError`, `DecodeError`) SHALL implement the standard Go error interface and support unwrapping.

#### Scenario: NetworkError wraps the underlying error
- **WHEN** a `*NetworkError` is returned
- **THEN** `errors.Unwrap()` SHALL return the underlying `net` error
- **AND** `errors.Is(err, context.DeadlineExceeded)` SHALL return true when the underlying error is a deadline exceeded

#### Scenario: DecodeError wraps the JSON decode error
- **WHEN** a `*DecodeError` is returned
- **THEN** `errors.Unwrap()` SHALL return the underlying JSON decode error

#### Scenario: StatusError has no wrapped error
- **WHEN** a `*StatusError` is returned
- **THEN** `errors.Unwrap()` SHALL return nil (status errors are terminal — they carry the HTTP status code and body as context, not a wrapped cause)

#### Scenario: Error messages include operation and URL
- **WHEN** any agent error type is converted to string via `.Error()`
- **THEN** the message SHALL include the operation name (e.g., "execute", "assign", "health_check") and the target URL for diagnostic context

### Requirement: AgentClient is safe for concurrent use

The `AgentClient` SHALL be safe for concurrent use from multiple goroutines without external synchronization.

#### Scenario: Concurrent Execute calls for different sessions
- **WHEN** multiple goroutines call `Execute` concurrently with different pod IPs and tokens
- **THEN** all calls SHALL complete without data races
- **AND** each call SHALL use the correct pod IP and token (no cross-contamination)

#### Scenario: Concurrent Execute and HealthCheck calls
- **WHEN** one goroutine calls `Execute` while another calls `HealthCheck` on the same pod IP
- **THEN** both calls SHALL complete without data races

### Requirement: AgentClient properly manages HTTP response body lifecycle

All methods SHALL close `resp.Body` on every code path and drain unconsumed bodies for connection reuse.

#### Scenario: Response body is closed on all paths
- **WHEN** any method (`Execute`, `Assign`, `HealthCheck`) receives an HTTP response
- **THEN** `resp.Body` SHALL be closed via `defer resp.Body.Close()` regardless of status code or decode outcome

#### Scenario: Unconsumed response body is drained for connection reuse
- **WHEN** `Assign` or `HealthCheck` receives a successful HTTP 200 response
- **AND** the response body is not consumed by JSON decoding
- **THEN** the body SHALL be drained (read to completion) before closing to allow the underlying TCP connection to be returned to the pool

### Requirement: AgentClient uses HTTP within the Kubernetes cluster trust boundary

The `AgentClient` SHALL use plain HTTP for communication with sandbox agents within the Kubernetes pod network.

#### Scenario: URLs use HTTP scheme
- **WHEN** any method constructs a URL for the agent
- **THEN** the URL scheme SHALL be `http://`
- **AND** the host SHALL be the pod IP with the configured port

#### Scenario: Trust boundary justification
- The MCP server and sandbox agents run in the same Kubernetes cluster and namespace
- **AND** NetworkPolicy restricts ingress to sandbox pods to only the MCP server pods (label `app: cli-mcp-server`) on port 8090
- **AND** the bearer token (HMAC-SHA256) authenticates the MCP server to the agent — it prevents unauthorized callers within the cluster, not network-level eavesdropping
- **AND** TLS/mTLS can be added later in `AgentClient` as a localized change if cross-cluster communication is needed
