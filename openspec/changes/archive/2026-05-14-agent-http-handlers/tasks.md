## 1. AgentState

- [ ] 1.1 Create `pkg/sandbox/handler.go` with `AgentState` struct (`mu sync.Mutex`, `token string`, `assigned bool`)
- [ ] 1.2 Implement `NewAgentState(token string) *AgentState` — if token non-empty, start assigned; otherwise unassigned
- [ ] 1.3 Implement `IsAssigned() bool` — acquire mutex, return `s.assigned`
- [ ] 1.4 Implement `GetToken() string` — acquire mutex, return `s.token`
- [ ] 1.5 Implement `Assign(token string) error` — acquire mutex, reject if already assigned, set token and assigned flag

## 2. Handler Struct and Health Endpoint

- [ ] 2.1 Add `Handler` struct (`session *BashSession`, `state *AgentState`)
- [ ] 2.2 Implement `NewHandler(session *BashSession, state *AgentState) *Handler`
- [ ] 2.3 Add `ErrorResponse` struct (`Error string`) and `writeError(w, status, msg)` helper
- [ ] 2.4 Implement `HandleHealth(w, r)` — call `session.IsAlive()`, return 200 `{"status":"ok"}` or 503 `{"status":"unhealthy"}`

## 3. Exec Endpoint

- [ ] 3.1 Implement `HandleExec(w, r)` — check `state.IsAssigned()`, return 503 if unassigned
- [ ] 3.2 Extract bearer token from `Authorization` header, strip `Bearer ` prefix, return 401 if missing/malformed
- [ ] 3.3 Compare token using `hmac.Equal([]byte(provided), []byte(state.GetToken()))`, return 401 if mismatch
- [ ] 3.4 Parse `agent.ExecRequest` from JSON body, return 400 if malformed
- [ ] 3.5 Validate command is non-empty, return 400 if empty
- [ ] 3.6 Convert timeout: `time.Duration(req.Timeout) * time.Second`, pass to `session.Execute()`
- [ ] 3.7 Map `ExecResult` to `agent.ExecResponse` (`DurationMs: result.Duration.Milliseconds()`)
- [ ] 3.8 Return 200 with JSON `ExecResponse`, or 500 on infrastructure error

## 4. Assign Endpoint

- [ ] 4.1 Implement `HandleAssign(w, r)` — parse `agent.AssignRequest` from JSON body
- [ ] 4.2 Validate token is non-empty, return 400 if empty
- [ ] 4.3 Call `state.Assign(token)`, return 409 if already assigned
- [ ] 4.4 Return 200 with empty body on success

## 5. Entry Point

- [ ] 5.1 Update `cmd/agent/main.go` — read `SANDBOX_AUTH_TOKEN` from env
- [ ] 5.2 Create `BashSession` with `NewBashSession(NewDefaultBashConfig())`, fatal on error
- [ ] 5.3 Create `AgentState` and `Handler`, log initial state (assigned vs unassigned)
- [ ] 5.4 Create `http.ServeMux` with Go 1.22+ method-based routing (`"POST /exec"`, `"POST /assign"`, `"GET /health"`)
- [ ] 5.5 Create `http.Server{Addr: ":8090", Handler: mux}`, start in goroutine
- [ ] 5.6 Set up `signal.NotifyContext` for `SIGTERM`/`SIGINT`, wait for signal
- [ ] 5.7 Call `srv.Shutdown()` with 310s timeout context
- [ ] 5.8 Call `session.Close()`, log shutdown complete
