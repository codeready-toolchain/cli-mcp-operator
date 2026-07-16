## ADDED Requirements

### Requirement: Cobra CLI exposes documented server flags

The `cli-mcp-server` binary SHALL use a Cobra root command with the following flags and defaults.

#### Scenario: Flag defaults
- **WHEN** the root command is defined
- **THEN** it SHALL provide:
  - `--address` / `-a` default `localhost:8080`
  - `--transport` / `-t` default `stdio` (values: `stdio`, `http`)
  - `--stateless` default `false`
  - `--namespace` default `tarsy`
  - `--sandbox-image` default empty (required before SessionManager construction)
  - `--kubeconfig` default empty
  - `--hmac-key-file` default empty (required; file contents become HMAC key)
  - `--idle-timeout` default `30m`
  - `--warm-pool-size` default `0`

#### Scenario: Required configuration rejected early
- **WHEN** `runServer` runs with empty `--sandbox-image` or empty/unreadable `--hmac-key-file`
- **THEN** it SHALL return a clear error before starting the HTTP listener
- **AND** it SHALL NOT create a SessionManager with invalid config

### Requirement: runServer bootstraps dependencies in a defined order

`runServer` SHALL initialize components in this order before accepting traffic:
1. Kubernetes clientset
2. `SessionManager` from `SandboxConfig` derived from flags
3. `mcp.Server` with middleware
4. Register `bash` tool
5. Start stale-session cleanup loop
6. Start warm pool reconciler when `warm-pool-size > 0`
7. Start transport (HTTP server or stdio)
8. Wait for shutdown signal

#### Scenario: Warm pool skipped when size is zero
- **WHEN** `--warm-pool-size` is `0`
- **THEN** `runServer` SHALL NOT start the warm pool reconciler

#### Scenario: Warm pool started when size positive
- **WHEN** `--warm-pool-size` is greater than `0`
- **THEN** `runServer` SHALL start the warm pool reconciler on a cancellable context

#### Scenario: Stale cleanup loop runs periodically
- **WHEN** the server is running
- **THEN** a background loop SHALL call `CleanupStale` on a 5-minute interval until shutdown cancels its context

### Requirement: HTTP transport uses http.Server with graceful Shutdown

For `--transport http`, the entry point SHALL serve the mux with `net/http.Server` and shut down via `Shutdown` on signal.

#### Scenario: SIGTERM triggers graceful shutdown
- **WHEN** the process receives SIGTERM
- **THEN** it SHALL cancel background contexts (cleanup/pool)
- **AND** it SHALL call `http.Server.Shutdown` with a timeout of at least 310 seconds (max bash timeout 300s + 10s buffer)
- **AND** it SHALL stop accepting new connections while draining in-flight requests

#### Scenario: SIGINT triggers the same shutdown path
- **WHEN** the process receives SIGINT
- **THEN** it SHALL follow the same graceful shutdown sequence as SIGTERM

### Requirement: stdio transport rejects stateless mode

#### Scenario: Stateless stdio is an error
- **WHEN** `--transport` is `stdio` and `--stateless` is true
- **THEN** `runServer` SHALL return an error and SHALL NOT start serving

### Requirement: Version is logged at startup

#### Scenario: Version printed on start
- **WHEN** the server binary starts
- **THEN** it SHALL print version information including commit and build time to stderr (existing `pkg/version` fields)
