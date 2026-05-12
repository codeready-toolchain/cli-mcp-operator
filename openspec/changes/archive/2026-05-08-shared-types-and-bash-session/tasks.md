## 1. Shared Types

- [ ] 1.1 Create `pkg/agent/types.go` with `ExecRequest` struct (`Command string`, `Timeout int`)
- [ ] 1.2 Add `ExecResponse` struct (`Stdout string`, `Stderr string`, `ExitCode int`, `DurationMs int64`)
- [ ] 1.3 Add `AssignRequest` struct (`Token string`)
- [ ] 1.4 Verify JSON tags match the parent design doc field names (`command`, `timeout`, `stdout`, `stderr`, `exit_code`, `duration_ms`, `token`)

## 2. BashSession Core

- [ ] 2.1 Create `pkg/sandbox/bash.go` with `ExecResult` struct (`Stdout string`, `Stderr string`, `ExitCode int`, `Duration time.Duration`)
- [ ] 2.2 Add `BashConfig` struct (currently empty) and `NewDefaultBashConfig()` function
- [ ] 2.3 Implement `NewBashSession(cfg BashConfig) (*BashSession, error)` — spawn `bash --norc --noprofile` with pipes, `Setpgid: true`, `PS1=`, `PS2=`
- [ ] 2.4 Implement `Close() error` — kill bash process, close pipes, idempotent
- [ ] 2.5 Implement `IsAlive() bool` — acquire mutex, return `!b.dead`

## 3. Command Execution

- [ ] 3.1 Implement `Execute(command string, timeout time.Duration) (*ExecResult, error)` with mutex acquisition
- [ ] 3.2 Implement UUID-based delimiter generation: `__SANDBOX_<uuid[:8]>__`
- [ ] 3.3 Implement wrapped command format: `echo DELIM_START`, command, `__exit=$?`, `echo DELIM_END`, `echo DELIM_EXIT_${__exit} >&2`
- [ ] 3.4 Implement concurrent pipe readers — two goroutines with `strings.Builder` and `sync.WaitGroup`
- [ ] 3.5 Implement `readUntilDelimiter()` helper — reads lines until delimiter, parses exit code from stderr marker
- [ ] 3.6 Measure wall-clock duration via `time.Since(start)`

## 4. Timeout Handling

- [ ] 4.1 Implement timeout using `time.After` alongside WaitGroup completion
- [ ] 4.2 On timeout: send `syscall.Kill(-pid, syscall.SIGKILL)` to process group
- [ ] 4.3 Wait up to 5 seconds for process to exit after SIGKILL
- [ ] 4.4 If process exits within 5s: collect partial output, return ExecResult with exit code 137
- [ ] 4.5 If process doesn't exit within 5s: mark session dead, return partial output with exit code 137

## 5. Crash Recovery

- [ ] 5.1 Detect bash crash via EOF on pipe reads or unexpected `cmd.Wait()` return
- [ ] 5.2 Set `b.dead = true` on crash detection
- [ ] 5.3 On next `Execute()` when `b.dead == true`: call `respawn()` to create new bash process
- [ ] 5.4 Implement `respawn() error` — same initialization as constructor, clears `dead` flag
- [ ] 5.5 Prepend `"[session state was reset — previous bash process exited]\n"` to stderr on respawn
- [ ] 5.6 Return error (not ExecResult) for the Execute() call that detects the crash
