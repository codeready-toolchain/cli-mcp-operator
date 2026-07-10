## 1. Typed Errors

- [ ] 1.1 Create `pkg/agent/errors.go` with `NetworkError` struct (`Op`, `URL`, `Err` fields), implementing `error` and `Unwrap()`
- [ ] 1.2 Add `StatusError` struct (`Op`, `URL`, `StatusCode`, `Body` fields), implementing `error` (no `Unwrap` — terminal error)
- [ ] 1.3 Add `DecodeError` struct (`Op`, `URL`, `StatusCode`, `Err`, `Body` fields), implementing `error` and `Unwrap()`
- [ ] 1.4 Error `Error()` messages include operation name and URL (e.g., `execute http://10.0.0.1:8090/exec: connection refused`)
- [ ] 1.5 `StatusError.Body` and `DecodeError.Body` are truncated to 512 bytes maximum

## 2. AgentClient — Core Structure

- [ ] 2.1 Create `pkg/agent/client.go` with `AgentClient` struct (`httpClient *http.Client`, `port int`, `maxResponseSize int64`)
- [ ] 2.2 Implement `NewAgentClient(opts ...Option)` with defaults: 30s timeout, port 8090, `DefaultMaxResponseSize` (10 MB)
- [ ] 2.3 Implement `WithTimeout(d time.Duration) Option` — sets `http.Client.Timeout`
- [ ] 2.4 Implement `WithHTTPClient(c *http.Client) Option` — replaces the entire HTTP client (for testing)
- [ ] 2.5 Implement `WithPort(port int) Option` — overrides the default agent port
- [ ] 2.6 Implement `WithMaxResponseSize(n int64) Option` — overrides the default maximum response body size for successful `Execute` responses
- [ ] 2.7 Add `buildURL(podIP, path string) string` helper — constructs `http://<podIP>:<port><path>` using `net.JoinHostPort`
- [ ] 2.8 Define `DefaultMaxResponseSize = 10 * 1024 * 1024` (10 MB) as a package-level constant

## 3. AgentClient — Execute Method

- [ ] 3.1 Implement `Execute(ctx context.Context, podIP, token string, req ExecRequest) (*ExecResponse, error)`
- [ ] 3.2 Marshal `ExecRequest` to JSON; return error on marshal failure (should not happen with valid types)
- [ ] 3.3 Create `POST` request with context, set `Content-Type: application/json` and `Authorization: Bearer <token>` headers
- [ ] 3.4 On `httpClient.Do` error, classify as `*NetworkError` with `Op: "execute"`
- [ ] 3.5 Immediately `defer resp.Body.Close()` after successful `httpClient.Do` — ensures body is closed on all paths (success, status error, decode error)
- [ ] 3.6 On non-200 status, read body (truncated to 512 bytes), return `*StatusError`
- [ ] 3.7 On 200 status, wrap `resp.Body` with `io.LimitReader(resp.Body, maxResponseSize)` before decoding JSON into `ExecResponse`; if the limited reader is exhausted (body exceeds cap), return `*DecodeError` indicating response too large
- [ ] 3.8 On decode failure (malformed JSON within size limit), return `*DecodeError` with raw body (truncated to 512 bytes)
- [ ] 3.9 On success, return `*ExecResponse`

## 4. AgentClient — Assign Method

- [ ] 4.1 Implement `Assign(ctx context.Context, podIP string, req AssignRequest) error`
- [ ] 4.2 Marshal `AssignRequest` to JSON
- [ ] 4.3 Create `POST` request with context, set `Content-Type: application/json`, no `Authorization` header
- [ ] 4.4 On `httpClient.Do` error, return `*NetworkError` with `Op: "assign"`
- [ ] 4.5 Immediately `defer resp.Body.Close()` after successful `httpClient.Do`
- [ ] 4.6 On non-200 status, return `*StatusError` with body (truncated to 512 bytes)
- [ ] 4.7 On 200 status, drain body with `io.Copy(io.Discard, resp.Body)` for connection reuse, then return nil

## 5. AgentClient — HealthCheck Method

- [ ] 5.1 Implement `HealthCheck(ctx context.Context, podIP string) error`
- [ ] 5.2 Create `GET` request with context, no auth headers
- [ ] 5.3 On `httpClient.Do` error, return `*NetworkError` with `Op: "health_check"`
- [ ] 5.4 Immediately `defer resp.Body.Close()` after successful `httpClient.Do`
- [ ] 5.5 On non-200 status, return `*StatusError` with body (truncated to 512 bytes)
- [ ] 5.6 On 200 status, drain body with `io.Copy(io.Discard, resp.Body)` for connection reuse, then return nil

## 6. Internal Helpers

- [ ] 6.1 Implement `readBodyTruncated(body io.Reader, limit int) string` — reads up to `limit` bytes and returns as string (caller is responsible for closing)
- [ ] 6.2 Implement `classifyDoError(err error, op, url string) *NetworkError` — wraps the error from `httpClient.Do` into a `*NetworkError`
- [ ] 6.3 Implement `drainAndClose(body io.ReadCloser)` — drains remaining body content (up to a small cap, e.g., 4 KB) into `io.Discard` then closes; used by `Assign` and `HealthCheck` success paths to enable connection reuse

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
- [ ] 7.26 Execute tests: response exceeding `DefaultMaxResponseSize` returns `*DecodeError`
- [ ] 7.27 Option tests: `WithMaxResponseSize` overrides the default cap
- [ ] 7.28 Concurrency tests: parallel Execute calls with different pod IPs complete without data races (run with `-race`)
