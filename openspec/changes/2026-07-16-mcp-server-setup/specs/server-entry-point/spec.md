## ADDED Requirements

### Requirement: Cobra CLI exposes documented server flags

The `cli-mcp-server` binary SHALL use a Cobra root command with the following flags and defaults.

#### Scenario: Flag defaults
- **WHEN** the root command is defined
- **THEN** it SHALL provide:
  - `--address` / `-a` default `localhost:8080`
  - `--transport` / `-t` default `stdio` (values: `stdio`, `http`)
  - `--stateless` default `false` (must be set `true` for HTTP)
  - `--namespace` default `tarsy`
  - `--sandbox-image` default empty (required for both transports)
  - `--kubeconfig` default empty
  - `--hmac-key-file` default empty (required for both transports; file contents become HMAC key)
  - `--idle-timeout` default `30m`
  - `--warm-pool-size` default `0`

#### Scenario: Required configuration rejected early for both transports
- **WHEN** `runServer` runs with empty `--sandbox-image` or empty/unreadable `--hmac-key-file`
- **THEN** it SHALL return a clear error before starting any transport
- **AND** it SHALL NOT create a SessionManager with invalid config
- **AND** this SHALL apply for both `--transport stdio` and `--transport http`

#### Scenario: Zero-length HMAC key file is rejected
- **WHEN** `--hmac-key-file` points to a readable file whose contents are zero-length
- **THEN** `runServer` SHALL return a clear error before constructing `SessionManager`
- **AND** it SHALL NOT start either transport
- **AND** this SHALL apply for both `--transport stdio` and `--transport http`

#### Scenario: Default invocation without required flags fails
- **WHEN** the binary is started with only default flag values (no `--sandbox-image`, no `--hmac-key-file`)
- **THEN** `runServer` SHALL fail validation and exit non-zero

### Requirement: HTTP requires stateless; stdio rejects stateless

#### Scenario: HTTP without --stateless is an error
- **WHEN** `--transport` is `http` and `--stateless` is false
- **THEN** `runServer` SHALL return an error and SHALL NOT start serving

#### Scenario: Stateless stdio is an error
- **WHEN** `--transport` is `stdio` and `--stateless` is true
- **THEN** `runServer` SHALL return an error and SHALL NOT start serving

#### Scenario: HTTP with --stateless succeeds validation for transport flags
- **WHEN** `--transport` is `http` and `--stateless` is true and other required flags are valid
- **THEN** transport-flag validation SHALL pass

### Requirement: runServer bootstraps dependencies in a defined order

`runServer` SHALL initialize components in this order before accepting traffic:
1. Validate flags (image, hmac key file, transport/stateless pairing, loopback address for HTTP)
2. Kubernetes clientset
3. `SessionManager` from `SandboxConfig` derived from flags
4. `mcp.Server` with middleware
5. Register `bash` tool
6. Start stale-session cleanup loop
7. Start warm pool reconciler when `warm-pool-size > 0`
8. Start transport (HTTP server or stdio)
9. Wait for shutdown signal **or** fatal serve error

#### Scenario: Warm pool skipped when size is zero
- **WHEN** `--warm-pool-size` is `0`
- **THEN** `runServer` SHALL NOT start the warm pool reconciler

#### Scenario: Warm pool started when size positive
- **WHEN** `--warm-pool-size` is greater than `0`
- **THEN** `runServer` SHALL start the warm pool reconciler on a cancellable context

#### Scenario: Stale cleanup loop runs periodically
- **WHEN** the server is running
- **THEN** a background loop SHALL call `CleanupStale` on a 5-minute interval until shutdown cancels its context

### Requirement: HTTP transport uses http.Server with graceful Shutdown and serve-error handling

For `--transport http`, the entry point SHALL serve the mux with `net/http.Server`, observe `ListenAndServe` results, and shut down via `Shutdown` on signal or fatal serve error.

#### Scenario: SIGTERM triggers graceful shutdown
- **WHEN** the process receives SIGTERM
- **THEN** it SHALL cancel background contexts (cleanup/pool)
- **AND** it SHALL call `http.Server.Shutdown` with a timeout of at least 310 seconds (max bash timeout 300s + 10s buffer)
- **AND** it SHALL stop accepting new connections while draining in-flight requests

#### Scenario: SIGINT triggers the same shutdown path
- **WHEN** the process receives SIGINT
- **THEN** it SHALL follow the same graceful shutdown sequence as SIGTERM

#### Scenario: Unexpected ListenAndServe failure stops the process
- **WHEN** `ListenAndServe` returns an error other than `http.ErrServerClosed` (e.g. address already in use)
- **THEN** `runServer` SHALL cancel background contexts
- **AND** it SHALL return that error (or a wrapped form)
- **AND** it SHALL NOT leave cleanup/pool workers running indefinitely without an HTTP listener

#### Scenario: ErrServerClosed is not a fatal serve failure
- **WHEN** `ListenAndServe` returns `http.ErrServerClosed` after a normal `Shutdown`
- **THEN** `runServer` SHALL treat it as successful shutdown, not as a serve failure

### Requirement: stdio transport blocks until cancel or transport error

For `--transport stdio`, `runServer` SHALL serve via `mcp.Server.Run` with `StdioTransport` on the shared cancellable context and SHALL stop background workers when the transport ends.

#### Scenario: stdio blocks on Run
- **WHEN** `--transport` is `stdio` and validation succeeds
- **THEN** `runServer` SHALL call `server.Run(ctx, &mcp.StdioTransport{})` and block until that call returns

#### Scenario: SIGTERM cancels stdio and stops workers
- **WHEN** the process receives SIGTERM (or SIGINT) while serving stdio
- **THEN** it SHALL cancel the shared context so `Run` returns
- **AND** it SHALL stop the stale-cleanup and warm-pool workers

#### Scenario: stdio transport error stops workers and surfaces
- **WHEN** `server.Run` returns a non-nil error
- **THEN** `runServer` SHALL stop background workers
- **AND** it SHALL return that error (or a wrapped form) to the caller

### Requirement: Version is logged at startup

#### Scenario: Version printed on start
- **WHEN** the server binary starts
- **THEN** it SHALL print version information including commit and build time to stderr (existing `pkg/version` fields)
