## ADDED Requirements

### Requirement: BashSession manages a persistent bash process
The `BashSession` struct SHALL manage a long-running `bash --norc --noprofile` process using stdin/stdout/stderr pipes, preserving state (environment variables, working directory, aliases) between Execute() calls.

#### Scenario: Environment variables persist between calls
- **WHEN** Execute is called with `export FOO=bar` followed by Execute with `echo $FOO`
- **THEN** the second call SHALL return stdout containing `bar`

#### Scenario: Working directory persists between calls
- **WHEN** Execute is called with `cd /tmp` followed by Execute with `pwd`
- **THEN** the second call SHALL return stdout containing `/tmp`

#### Scenario: Bash started with no user configuration
- **WHEN** NewBashSession is called
- **THEN** bash SHALL be started with `--norc --noprofile` flags and `PS1=`, `PS2=` environment variables to suppress prompts

### Requirement: BashSession initializes eagerly
The `NewBashSession(cfg BashConfig)` constructor SHALL start the bash process immediately and return an error if bash fails to start.

#### Scenario: Constructor starts bash process
- **WHEN** NewBashSession is called with a valid BashConfig
- **THEN** a bash process SHALL be running and pipes SHALL be open

#### Scenario: Constructor fails if bash unavailable
- **WHEN** NewBashSession is called but bash is not in PATH
- **THEN** it SHALL return a non-nil error

#### Scenario: IsAlive returns true after construction
- **WHEN** NewBashSession succeeds
- **THEN** IsAlive() SHALL return true

### Requirement: Execute uses UUID-based delimiter protocol
Each Execute() call SHALL wrap the command with unique UUID-based delimiters to isolate its output from the continuous bash stream.

#### Scenario: Delimiter wraps command output
- **WHEN** Execute is called with command `echo hello`
- **THEN** the wrapped command written to stdin SHALL include `echo <DELIM>_START`, the command, exit code capture, `echo <DELIM>_END`, and `echo <DELIM>_EXIT_${__exit} >&2`

#### Scenario: Delimiter is unique per call
- **WHEN** Execute is called twice
- **THEN** each call SHALL generate a different delimiter using `uuid.NewString()[:8]`

#### Scenario: Stdout is captured between START and END delimiters
- **WHEN** a command produces stdout output
- **THEN** Execute SHALL return only the content between `<DELIM>_START` and `<DELIM>_END` in the Stdout field

#### Scenario: Exit code is parsed from stderr delimiter
- **WHEN** a command exits with code 2
- **THEN** Execute SHALL parse the exit code from the `<DELIM>_EXIT_2` marker on stderr and return `ExitCode: 2`

### Requirement: Execute reads stdout and stderr concurrently
Execute() SHALL read stdout and stderr using two concurrent goroutines writing to shared `strings.Builder` buffers, synchronized with `sync.WaitGroup`, to avoid pipe buffer deadlocks.

#### Scenario: Command writing to both streams does not deadlock
- **WHEN** a command writes heavily to both stdout and stderr simultaneously
- **THEN** Execute SHALL complete without deadlocking on pipe buffer pressure

#### Scenario: Stderr is captured separately from stdout
- **WHEN** a command writes `output` to stdout and `warning` to stderr
- **THEN** Execute SHALL return `Stdout` containing `output` and `Stderr` containing `warning`

### Requirement: Execute is serialized via mutex
Only one command SHALL execute at a time. Execute() SHALL acquire a mutex before any operation and release it when complete.

#### Scenario: Concurrent Execute calls are serialized
- **WHEN** two goroutines call Execute simultaneously
- **THEN** the second call SHALL block until the first completes

### Requirement: Execute handles command timeout
When a command exceeds its timeout, Execute SHALL send SIGKILL to the command's process group (not bash itself), wait up to 5 seconds for the process to exit, and return partial output with exit code 137.

#### Scenario: Timed-out command is killed
- **WHEN** Execute is called with `sleep 60` and timeout of 1 second
- **THEN** the command SHALL be killed via SIGKILL to the process group and ExitCode SHALL be 137

#### Scenario: Bash survives after timeout
- **WHEN** a command is killed due to timeout
- **THEN** the next Execute call SHALL succeed because bash itself was not killed

#### Scenario: Partial output is returned on timeout
- **WHEN** a command produces some output before being timed out
- **THEN** Execute SHALL return whatever output was captured before the timeout in Stdout/Stderr

#### Scenario: Unkillable process triggers crash recovery
- **WHEN** SIGKILL is sent but the process does not exit within 5 seconds
- **THEN** the session SHALL be marked dead and crash recovery SHALL trigger on the next Execute call

### Requirement: Execute handles bash crash with deferred recovery
When bash dies during Execute (detected via EOF on pipe reads), the current call SHALL return an error. The next Execute call SHALL respawn bash and prepend a session reset notice to stderr.

#### Scenario: Current call returns error on crash
- **WHEN** the bash process is killed externally during an Execute call
- **THEN** Execute SHALL detect EOF on the pipes, mark the session as dead, and return an error

#### Scenario: Next call respawns bash
- **WHEN** Execute is called after a crash
- **THEN** it SHALL respawn a new bash process and execute the command in the new session

#### Scenario: Session reset notice is prepended to stderr
- **WHEN** Execute runs in a respawned bash session
- **THEN** the Stderr field SHALL be prepended with `[session state was reset — previous bash process exited]\n`

#### Scenario: IsAlive returns false when bash is dead
- **WHEN** the bash process has crashed
- **THEN** IsAlive() SHALL return false

#### Scenario: IsAlive returns true after respawn
- **WHEN** Execute successfully respawns bash after a crash
- **THEN** IsAlive() SHALL return true

### Requirement: Execute returns ExecResult with duration
Execute() SHALL return an `ExecResult` struct containing `Stdout`, `Stderr`, `ExitCode`, and `Duration` (as `time.Duration`). Duration SHALL measure wall-clock time from writing the command to receiving the exit code.

#### Scenario: Duration measures wall-clock time
- **WHEN** Execute is called with `sleep 1`
- **THEN** the Duration field SHALL be approximately 1 second

#### Scenario: Non-zero exit codes are valid results
- **WHEN** a command returns exit code 1
- **THEN** Execute SHALL return an ExecResult (not an error) with ExitCode 1

#### Scenario: Only infrastructure failures return errors
- **WHEN** bash crashes or stdin write fails
- **THEN** Execute SHALL return an error (not an ExecResult)

### Requirement: Output is returned as-is
Execute() SHALL return command output without any truncation, transformation, or size limits. Output treatment is TARSy's responsibility.

#### Scenario: Large output is not truncated
- **WHEN** a command produces 5MB of stdout
- **THEN** Execute SHALL return all 5MB in the Stdout field without truncation

#### Scenario: Binary-like output is preserved
- **WHEN** a command produces non-UTF-8 output
- **THEN** Execute SHALL return the raw bytes as a string without modification

### Requirement: BashSession can be closed gracefully
The `Close()` method SHALL kill the bash process and close all pipes. It SHALL be idempotent.

#### Scenario: Close kills bash process
- **WHEN** Close is called on an active BashSession
- **THEN** the bash process SHALL be terminated and all pipes SHALL be closed

#### Scenario: Close is idempotent
- **WHEN** Close is called twice
- **THEN** the second call SHALL return without error

#### Scenario: IsAlive returns false after Close
- **WHEN** Close has been called
- **THEN** IsAlive() SHALL return false

### Requirement: IsAlive is thread-safe
The `IsAlive()` method SHALL acquire the mutex before reading the `dead` flag to ensure thread safety with concurrent Execute() calls.

#### Scenario: Concurrent IsAlive and Execute
- **WHEN** IsAlive is called while Execute is running
- **THEN** IsAlive SHALL block until the mutex is available and return a consistent result

### Requirement: BashSession uses process group isolation
The bash process SHALL be started with `SysProcAttr{Setpgid: true}` so that SIGKILL can target a command's process group without killing bash itself.

#### Scenario: Process group is set on bash
- **WHEN** NewBashSession creates the bash process
- **THEN** `cmd.SysProcAttr` SHALL have `Setpgid: true`
