## 1. Dependencies

- [ ] 1.1 Add `github.com/codeready-toolchain/mcp-common` (middleware + shared version patterns as needed)
- [ ] 1.2 Add `github.com/spf13/cobra`
- [ ] 1.3 Add `github.com/prometheus/client_golang` (`promhttp`)
- [ ] 1.4 Run `go mod tidy` and confirm builds against go-sdk v1.4.1+

## 2. `pkg/server` — MCP server and HTTP mux

- [ ] 2.1 Create `pkg/server/server.go` with package `server`
- [ ] 2.2 Implement `NewMCPServer(name string, stateless bool, logger *slog.Logger) *mcp.Server` — Capabilities with `Tools.ListChanged: !stateless`, `MetricsMiddleware` + `LoggingMiddleware`
- [ ] 2.3 Implement mux builder that registers:
  - `/mcp` — `mcp.NewStreamableHTTPHandler` with `Stateless` from config and `DisableLocalhostProtection: true`
  - `DELETE /sessions/{id}` — session delete handler
  - `/metrics` — `promhttp.Handler()`
  - `/live` — liveness JSON
  - `/health` — readiness via K8s API check
- [ ] 2.4 Implement session delete handler: empty id → 400; `CleanupSession` error → 500; success → 204
- [ ] 2.5 Implement `/health` with ≤5s timeout context; namespace get (or equivalent lightweight check); 503 + JSON error when unreachable; 200 + healthy JSON when OK
- [ ] 2.6 Expose a narrow interface for cleanup/health dependencies so tests can inject fakes

## 3. `cmd/server` — Cobra entry point

- [ ] 3.1 Replace stub in `cmd/server/main.go` with Cobra root command
- [ ] 3.2 Add flags: `--address`, `--transport`, `--stateless`, `--namespace`, `--sandbox-image`, `--kubeconfig`, `--hmac-key-file`, `--idle-timeout`, `--warm-pool-size`
- [ ] 3.3 Implement `runServer`: build logger; load HMAC key file; construct Kubernetes clientset (in-cluster or kubeconfig); build `SandboxConfig`; `NewSessionManager`
- [ ] 3.4 Create mcp.Server via `pkg/server`, register `tools.NewBashTool(mgr).RegisterWith(server)`
- [ ] 3.5 Start stale cleanup goroutine (5m ticker → `CleanupStale`) on cancellable context
- [ ] 3.6 Call `mgr.StartPool(ctx)` when `WarmPoolSize > 0`
- [ ] 3.7 For HTTP transport: create `http.Server` with mux, listen in goroutine; for stdio: `server.Run` with `StdioTransport` (stateless must be false)
- [ ] 3.8 Handle SIGTERM/SIGINT: cancel background ctx, `http.Server.Shutdown` with timeout (≥ max bash timeout + buffer, prefer 310s), exit cleanly

## 4. Unit tests (`pkg/server`)

- [ ] 4.1 Create `pkg/server/server_test.go` with given/when/then + testify
- [ ] 4.2 Test: bash tool registered (ListTools or equivalent) after wiring helper
- [ ] 4.3 Test: `DELETE /sessions/{id}` → 204 and cleanup called with that id
- [ ] 4.4 Test: `DELETE /sessions/` or missing id → 400
- [ ] 4.5 Test: cleanup error → 500
- [ ] 4.6 Test: `/live` → 200 and alive status
- [ ] 4.7 Test: `/health` success → 200 healthy; failure → 503 unhealthy
- [ ] 4.8 Run `go test ./pkg/server/...` and `go build ./cmd/server/`

## 5. Verification against acceptance criteria

- [ ] 5.1 Confirm mcp.Server uses MetricsMiddleware + LoggingMiddleware (mcp-server-devsandbox pattern)
- [ ] 5.2 Confirm StreamableHTTPHandler uses Stateless (when configured) + DisableLocalhostProtection true
- [ ] 5.3 Confirm mux routes: `/mcp`, `DELETE /sessions/{id}` → 204, `/metrics`, `/live`, `/health` (K8s API)
- [ ] 5.4 Confirm Cobra flags match design (including transport/stateless)
- [ ] 5.5 Confirm graceful shutdown on SIGTERM
