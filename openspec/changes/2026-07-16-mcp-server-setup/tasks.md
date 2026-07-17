## 1. Dependencies

- [ ] 1.1 Add `github.com/codeready-toolchain/mcp-common` (middleware + shared version patterns as needed)
- [ ] 1.2 Add `github.com/spf13/cobra`
- [ ] 1.3 Add `github.com/prometheus/client_golang` (`promhttp`)
- [ ] 1.4 Run `go mod tidy` and confirm builds against go-sdk v1.4.1+

## 2. `pkg/server` — MCP server and HTTP mux

- [ ] 2.1 Create `pkg/server/server.go` with package `server`
- [ ] 2.2 Implement `NewMCPServer(name string, stateless bool, logger *slog.Logger) *mcp.Server` — Capabilities with `Tools.ListChanged: !stateless`, `MetricsMiddleware` + `LoggingMiddleware`
- [ ] 2.3 Implement mux builder that registers:
  - `/mcp` — `mcp.NewStreamableHTTPHandler` with `Stateless: true` (HTTP) and `DisableLocalhostProtection: true`
  - `DELETE /sessions/{id}` — session delete handler
  - `DELETE /sessions/` — empty-id fallback returning 400 without CleanupSession
  - `/metrics` — `promhttp.Handler()`
  - `/live` — liveness JSON
  - `/health` — readiness via K8s API check
- [ ] 2.4 Implement session delete handler: empty id → 400; success → 204; cleanup error → generic 500 body (no underlying error text) + server-side log with session ID
- [ ] 2.5 Implement `/health` with ≤5s timeout context; namespace get (or equivalent lightweight check); 503 + JSON error when unreachable; 200 + healthy JSON when OK
- [ ] 2.6 Expose a narrow interface for cleanup/health dependencies so tests can inject fakes
- [ ] 2.7 Provide a loopback-address helper used by `runServer` validation (host is `127.0.0.1`, `localhost`, or `::1`)

## 3. `cmd/server` — Cobra entry point

- [ ] 3.1 Replace stub in `cmd/server/main.go` with Cobra root command
- [ ] 3.2 Add flags: `--address`, `--transport`, `--stateless`, `--namespace`, `--sandbox-image`, `--kubeconfig`, `--hmac-key-file`, `--idle-timeout`, `--warm-pool-size`
- [ ] 3.3 Validate before listen: non-empty `--sandbox-image`; readable `--hmac-key-file` with **contents length > 0** (reject zero-byte files); `http` requires `--stateless` and loopback `--address`; `stdio` rejects `--stateless`
- [ ] 3.4 Implement `runServer`: build logger; load HMAC key file; construct Kubernetes clientset (in-cluster or kubeconfig); build `SandboxConfig`; `NewSessionManager`
- [ ] 3.5 Create mcp.Server via `pkg/server`, register `tools.NewBashTool(mgr).RegisterWith(server)`
- [ ] 3.6 Start stale cleanup goroutine (5m ticker → `CleanupStale`) on cancellable context
- [ ] 3.7 Call `mgr.StartPool(ctx)` when `WarmPoolSize > 0`
- [ ] 3.8 For HTTP transport: create `http.Server` with mux; start `ListenAndServe` in a goroutine; `select` on signal context **or** serve-error channel; treat non-`http.ErrServerClosed` as fatal (cancel ctx, return error)
- [ ] 3.9 For stdio transport: `server.Run(ctx, &mcp.StdioTransport{})` on shared cancellable ctx; on return (cancel or error) stop cleanup/pool workers; surface non-nil `Run` errors
- [ ] 3.10 On SIGTERM/SIGINT (HTTP): cancel background ctx, `http.Server.Shutdown` with timeout (≥ max bash timeout + buffer, prefer 310s), exit cleanly

## 4. Unit tests (`pkg/server`)

- [ ] 4.1 Create `pkg/server/server_test.go` with given/when/then + testify
- [ ] 4.2 Test: bash tool registered (ListTools or equivalent) after wiring helper
- [ ] 4.3 Test: `DELETE /sessions/{id}` → 204 and cleanup called with that id
- [ ] 4.4 Test: `DELETE /sessions/` → 400 and cleanup not called (not 404)
- [ ] 4.5 Test: cleanup error → 500 with generic body (underlying error text not present); log path optional to assert
- [ ] 4.6 Test: `/live` → 200 and alive status
- [ ] 4.7 Test: `/health` success → 200 healthy; failure → 503 unhealthy
- [ ] 4.8 Test: loopback helper accepts localhost/127.0.0.1/::1 and rejects non-loopback
- [ ] 4.9 Run `go test ./pkg/server/...` and `go build ./cmd/server/`

## 5. Verification against acceptance criteria

- [ ] 5.1 Confirm mcp.Server uses MetricsMiddleware + LoggingMiddleware (mcp-server-devsandbox pattern)
- [ ] 5.2 Confirm StreamableHTTPHandler uses Stateless true for HTTP + DisableLocalhostProtection true
- [ ] 5.3 Confirm mux routes: `/mcp`, `DELETE /sessions/{id}` → 204, `DELETE /sessions/` → 400, `/metrics`, `/live`, `/health` (K8s API)
- [ ] 5.4 Confirm Cobra flags match design; HTTP requires `--stateless` + loopback address; both transports require image + non-empty hmac key contents (including reject zero-byte file)
- [ ] 5.5 Confirm graceful shutdown on SIGTERM; fatal exit on unexpected ListenAndServe errors; stdio Run cancel/error stops workers
