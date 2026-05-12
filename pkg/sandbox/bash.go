package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultTimeout = 60 * time.Second
	MaxTimeout     = 300 * time.Second

	killWaitTimeout = 5 * time.Second

	sessionResetNotice = "[session state was reset — previous bash process exited]\n"
)

// ExecResult is the domain-specific result of a command execution.
// It stays in pkg/sandbox with no dependency on the wire-format types in pkg/agent.
// The HTTP handler maps this to agent.ExecResponse (~5 lines).
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// BashConfig holds configuration for a BashSession.
// Currently empty — provides a stable constructor extension point.
type BashConfig struct{}

// NewDefaultBashConfig returns a BashConfig with default settings.
func NewDefaultBashConfig() BashConfig {
	return BashConfig{}
}

// BashSession manages a persistent bash process with stdin/stdout/stderr pipes.
// Commands are isolated using a UUID-based delimiter protocol.
type BashSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bufio.Reader
	mu     sync.Mutex
	dead   bool
}

// NewBashSession spawns a persistent bash process.
func NewBashSession(cfg BashConfig) (*BashSession, error) {
	bs := &BashSession{}
	if err := bs.spawn(); err != nil {
		return nil, fmt.Errorf("failed to start bash: %w", err)
	}
	return bs, nil
}

func (bs *BashSession) spawn() error {
	cmd := exec.Command("bash", "--norc", "--noprofile")
	cmd.Env = append(os.Environ(), "PS1=", "PS2=")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bash: %w", err)
	}

	bs.cmd = cmd
	bs.stdin = stdin
	bs.stdout = bufio.NewReader(stdoutPipe)
	bs.stderr = bufio.NewReader(stderrPipe)
	bs.dead = false

	return nil
}

// reapProcess kills and waits on the current bash process so it doesn't
// become a zombie. Must be called with bs.mu held.
func (bs *BashSession) reapProcess() {
	if bs.cmd == nil || bs.cmd.Process == nil {
		return
	}
	if bs.stdin != nil {
		bs.stdin.Close() //nolint:errcheck
	}
	_ = syscall.Kill(-bs.cmd.Process.Pid, syscall.SIGKILL)
	bs.cmd.Wait() //nolint:errcheck
}

// IsAlive reports whether the bash process is running.
func (bs *BashSession) IsAlive() bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return !bs.dead
}

// Close terminates the bash process. It is idempotent.
func (bs *BashSession) Close() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if !bs.dead {
		bs.dead = true
		bs.reapProcess()
	}
	return nil
}

// Execute runs a command in the persistent bash session with the given timeout.
// If the bash process has crashed, it respawns automatically and reports the reset in stderr.
func (bs *BashSession) Execute(command string, timeout time.Duration) (*ExecResult, error) {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}

	stateReset := false
	if bs.dead {
		bs.reapProcess()
		if err := bs.spawn(); err != nil {
			return nil, fmt.Errorf("failed to respawn bash: %w", err)
		}
		stateReset = true
	}

	delimiter := fmt.Sprintf("__SANDBOX_%s__", uuid.NewString()[:8])
	startMarker := delimiter + "_START"
	endMarker := delimiter + "_END"
	exitMarker := delimiter + "_EXIT_"

	// Wrapped command isolates user stdin via </dev/null so commands like
	// cat or read cannot consume the protocol markers that follow.
	// Leading : (no-op) ensures the group is valid even for empty commands.
	wrapped := fmt.Sprintf(
		"echo %s\n{ :\n%s\n} </dev/null\n__exit=$?\necho %s\necho %s${__exit} >&2\n",
		startMarker, command, endMarker, exitMarker,
	)

	start := time.Now()

	if _, err := io.WriteString(bs.stdin, wrapped); err != nil {
		if stateReset {
			return nil, fmt.Errorf("write to stdin: %w", err)
		}
		bs.dead = true
		bs.reapProcess()
		if err := bs.spawn(); err != nil {
			return nil, fmt.Errorf("failed to respawn bash: %w", err)
		}
		stateReset = true
		if _, err := io.WriteString(bs.stdin, wrapped); err != nil {
			bs.dead = true
			return nil, fmt.Errorf("write to stdin after respawn: %w", err)
		}
	}

	type readResult struct {
		output   string
		exitCode int
		err      error
	}

	stdoutCh := make(chan readResult, 1)
	stderrCh := make(chan readResult, 1)

	stdoutReader := bs.stdout
	stderrReader := bs.stderr

	go func() {
		var buf strings.Builder
		capturing := false
		for {
			line, err := stdoutReader.ReadString('\n')
			if strings.Contains(line, startMarker) {
				capturing = true
			} else if strings.Contains(line, endMarker) {
				stdoutCh <- readResult{output: strings.TrimRight(buf.String(), "\n")}
				return
			} else if capturing {
				buf.WriteString(line)
			}
			if err != nil {
				stdoutCh <- readResult{output: strings.TrimRight(buf.String(), "\n"), err: err}
				return
			}
		}
	}()

	go func() {
		var buf strings.Builder
		for {
			line, err := stderrReader.ReadString('\n')
			if strings.Contains(line, exitMarker) {
				codeStr := line[strings.Index(line, exitMarker)+len(exitMarker):]
				codeStr = strings.TrimSpace(codeStr)
				code, _ := strconv.Atoi(codeStr)
				stderrCh <- readResult{output: strings.TrimRight(buf.String(), "\n"), exitCode: code}
				return
			}
			if line != "" {
				buf.WriteString(line)
			}
			if err != nil {
				stderrCh <- readResult{output: strings.TrimRight(buf.String(), "\n"), exitCode: -1, err: err}
				return
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var stdoutStr, stderrStr string
	exitCode := 0
	completed := 0

	for completed < 2 {
		select {
		case res := <-stdoutCh:
			stdoutStr = res.output
			if res.err != nil {
				bs.dead = true
			}
			completed++
		case res := <-stderrCh:
			stderrStr = res.output
			if res.exitCode >= 0 {
				exitCode = res.exitCode
			} else {
				exitCode = 137
				bs.dead = true
			}
			completed++
		case <-timer.C:
			if bs.cmd != nil && bs.cmd.Process != nil {
				if err := syscall.Kill(-bs.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					_ = err
				}
				bs.cmd.Wait() //nolint:errcheck
			}
			bs.dead = true

			drainTimeout := time.After(killWaitTimeout)
			for completed < 2 {
				select {
				case res := <-stdoutCh:
					stdoutStr = res.output
					completed++
				case res := <-stderrCh:
					stderrStr = res.output
					completed++
				case <-drainTimeout:
					completed = 2
				}
			}

			duration := time.Since(start)
			timeoutStderr := stderrStr
			if timeoutStderr != "" {
				timeoutStderr += "\ncommand timed out"
			} else {
				timeoutStderr = "command timed out"
			}

			return &ExecResult{
				Stdout:   stdoutStr,
				Stderr:   timeoutStderr,
				ExitCode: 137,
				Duration: duration,
			}, nil
		}
	}

	duration := time.Since(start)

	if bs.dead {
		return nil, fmt.Errorf("bash process exited unexpectedly")
	}

	if stateReset {
		if stderrStr != "" {
			stderrStr = sessionResetNotice + stderrStr
		} else {
			stderrStr = strings.TrimRight(sessionResetNotice, "\n")
		}
	}

	return &ExecResult{
		Stdout:   stdoutStr,
		Stderr:   stderrStr,
		ExitCode: exitCode,
		Duration: duration,
	}, nil
}
