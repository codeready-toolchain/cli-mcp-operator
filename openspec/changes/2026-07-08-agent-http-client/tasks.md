## 1. Typed Errors

- [ ] 1.1 Create `pkg/agent/errors.go` with `NetworkError` struct (`Op`, `URL`, `Err` fields), implementing `error` and `Unwrap()`
- [ ] 1.2 Add `StatusError` struct (`Op`, `URL`, `StatusCode`, `Body` fields), implementing `error` (no `Unwrap` — terminal error)
- [ ] 1.3 Add `DecodeError` struct (`Op`, `URL`, `StatusCode`, `Err`, `Body` fields), implementing `error` and `Unwrap()`
- [ ] 1.4 Error `Error()` messages include operation name and URL (e.g., `execute http://10.0.0.1:8090/exec: connection refused`)
- [ ] 1.5 `StatusError.Body` and `DecodeError.Body` are truncated to 512 bytes maximum

## 2. AgentClient — Core Structure

- [ ] 2.1 Create `pkg/agent/client.go` with `AgentClient` struct (`httpClient *http.Client`, `port int`)
- [ ] 2.2 Implement `NewAgentClient(opts ...Option)` with defaults: 30s timeout, port 8090
- [ ] 2.3 Implement `WithTimeout(d time.Duration) Option` — sets `http.Client.Timeout`
- [ ] 2.4 Implement `WithHTTPClient(c *http.Client) Option` — replaces the entire HTTP client (for testing)
- [ ] 2.5 Implement `WithPort(port int) Option` — overrides the default agent port
- [ ] 2.6 Add `buildURL(podIP, path string) string` helper — constructs `http://<podIP>:<port><path>` using `net.JoinHostPort`

## 3. AgentClient — Execute Method

- [ ] 3.1 Implement `Execute(ctx context.Context, podIP, token string, req ExecRequest) (*ExecResponse, error)`
- [ ] 3.2 Marshal `ExecRequest` to JSON; return error on marshal failure (should not happen with valid types)
- [ ] 3.3 Create `POST` request with context, set `Content-Type: application/json` and `Authorization: Bearer <token>` headers
- [ ] 3.4 On `httpClient.Do` error, classify as `*NetworkError` with `Op: "execute"`
- [ ] 3.5 On non-200 status, read body (truncated to 512 bytes), return `*StatusError`
- [ ] 3.6 On 200 status, decode JSON into `ExecResponse`; on decode failure, return `*DecodeError` with raw body (truncated to 512 bytes)
- [ ] 3.7 On success, return `*ExecResponse`

## 4. AgentClient — Assign Method

- [ ] 4.1 Implement `Assign(ctx context.Context, podIP string, req AssignRequest) error`
- [ ] 4.2 Marshal `AssignRequest` to JSON
- [ ] 4.3 Create `POST` request with context, set `Content-Type: application/json`, no `Authorization` header
- [ ] 4.4 On `httpClient.Do` error, return `*NetworkError` with `Op: "assign"`
- [ ] 4.5 On non-200 status, return `*StatusError` with body (truncated to 512 bytes)
- [ ] 4.6 On 200 status, return nil

## 5. AgentClient — HealthCheck Method

- [ ] 5.1 Implement `HealthCheck(ctx context.Context, podIP string) error`
- [ ] 5.2 Create `GET` request with context, no auth headers
- [ ] 5.3 On `httpClient.Do` error, return `*NetworkError` with `Op: "health_check"`
- [ ] 5.4 On non-200 status, return `*StatusError` with body (truncated to 512 bytes)
- [ ] 5.5 On 200 status, return nil (body is not read — health check is a simple liveness signal)

## 6. Internal Helpers

- [ ] 6.1 Implement `readBodyTruncated(body io.ReadCloser, limit int) string` — reads up to `limit` bytes and returns as string; closes the body
- [ ] 6.2 Implement `classifyDoError(err error, op, url string) *NetworkError` — wraps the error from `httpClient.Do` into a `*NetworkError`

## 7. Unit Tests

- [ ] 7.1 Create `pkg/agent/client_test.go` with `httptest.NewServer` for all test cases
- [ ] 7.2 Execute tests: successful execution (200 + valid JSON → `*ExecResponse`)
- [ ] 7.3 Execute tests: non-200 response (e.g., 401, 500 → `*StatusError` with correct code and truncated body)
- [ ] 7.4 Execute tests: malformed JSON response (200 + invalid JSON → `*DecodeError`)
- [ ] 7.5 Execute tests: connection refused (server not listening → `*NetworkError`)
- [ ] 7.6 Execute tests: context timeout (slow server + short context deadline → `*NetworkError` wrapping `context.DeadlineExceeded`)
- [ ] 7.7 Execute tests: context cancellation (cancelled context → `*NetworkError` wrapping `context.Canceled`)
- [ ] 7.8 Execute tests: bearer token is sent in Authorization header
- [ ] 7.9 Execute tests: request body matches ExecRequest JSON encoding
- [ ] 7.10 Assign tests: successful assignment (200 → nil error)
- [ ] 7.11 Assign tests: 409 Conflict (already assigned → `*StatusError` with code 409)
- [ ] 7.12 Assign tests: transport failure → `*NetworkError`
- [ ] 7.13 Assign tests: no Authorization header is sent
- [ ] 7.14 HealthCheck tests: healthy (200 → nil error)
- [ ] 7.15 HealthCheck tests: unhealthy (503 → `*StatusError`)
- [ ] 7.16 HealthCheck tests: unreachable → `*NetworkError`
- [ ] 7.17 Option tests: default timeout is 30s
- [ ] 7.18 Option tests: `WithTimeout` overrides timeout
- [ ] 7.19 Option tests: `WithPort` overrides port in URL
- [ ] 7.20 Option tests: `WithHTTPClient` replaces the internal client
- [ ] 7.21 Error tests: `errors.As(err, &NetworkError{})` works
- [ ] 7.22 Error tests: `errors.As(err, &StatusError{})` works
- [ ] 7.23 Error tests: `errors.As(err, &DecodeError{})` works
- [ ] 7.24 Error tests: `errors.Is(networkErr, context.DeadlineExceeded)` works through unwrap chain
- [ ] 7.25 Error tests: `StatusError.Body` is truncated to 512 bytes for large responses
- [ ] 7.26 Concurrency tests: parallel Execute calls with different pod IPs complete without data races (run with `-race`)
