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

	"github.com/codeready-toolchain/cli-mcp-server/pkg/agent"
)

const (
	DefaultTimeout = 60 * time.Second
	MaxTimeout     = 300 * time.Second
)

// BashSession manages a persistent bash process with stdin/stdout/stderr pipes.
// Commands are isolated using a UUID-based delimiter protocol.
type BashSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bufio.Reader
	mu     sync.Mutex
	alive  bool
}

// NewBashSession spawns a persistent bash process.
func NewBashSession() (*BashSession, error) {
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
	bs.alive = true

	return nil
}

// Alive reports whether the bash process is running.
func (bs *BashSession) Alive() bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.alive
}

// Close terminates the bash process.
func (bs *BashSession) Close() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.alive = false
	var errs []error
	if bs.stdin != nil {
		if err := bs.stdin.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close stdin: %w", err))
		}
	}
	if bs.cmd != nil && bs.cmd.Process != nil {
		if err := bs.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			errs = append(errs, fmt.Errorf("kill bash: %w", err))
		}
		bs.cmd.Wait() //nolint:errcheck
	}
	return errors.Join(errs...)
}

// Execute runs a command in the persistent bash session with the given timeout.
// If the bash process has crashed, it respawns automatically and reports the reset in stderr.
func (bs *BashSession) Execute(command string, timeout time.Duration) (*agent.ExecResponse, error) {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}

	stateReset := false
	if !bs.alive {
		if err := bs.spawn(); err != nil {
			return nil, fmt.Errorf("failed to respawn bash: %w", err)
		}
		stateReset = true
	}

	delimiter := fmt.Sprintf("__SANDBOX_%s__", uuid.NewString()[:8])
	startMarker := delimiter + "_START"
	endMarker := delimiter + "_END"
	exitMarker := delimiter + "_EXIT_"

	// Wrapped command:
	//   echo <START>
	//   <command>
	//   __exit=$?
	//   echo <END>
	//   echo <EXIT_$__exit> >&2
	wrapped := fmt.Sprintf(
		"echo %s\n%s\n__exit=$?\necho %s\necho %s${__exit} >&2\n",
		startMarker, command, endMarker, exitMarker,
	)

	start := time.Now()

	if _, err := io.WriteString(bs.stdin, wrapped); err != nil {
		bs.alive = false
		return nil, fmt.Errorf("write to stdin: %w", err)
	}

	type readResult struct {
		lines    []string
		exitCode int
		err      error
	}

	stdoutCh := make(chan readResult, 1)
	stderrCh := make(chan readResult, 1)

	// Capture readers locally so goroutines don't race with spawn()
	stdoutReader := bs.stdout
	stderrReader := bs.stderr

	// Read stdout until end marker
	go func() {
		var lines []string
		capturing := false
		scanner := bufio.NewScanner(stdoutReader)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, startMarker) {
				capturing = true
				continue
			}
			if strings.Contains(line, endMarker) {
				stdoutCh <- readResult{lines: lines}
				return
			}
			if capturing {
				lines = append(lines, line)
			}
		}
		stdoutCh <- readResult{lines: lines, err: scanner.Err()}
	}()

	// Read stderr until exit marker
	go func() {
		var lines []string
		scanner := bufio.NewScanner(stderrReader)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, exitMarker) {
				codeStr := strings.TrimPrefix(line, exitMarker)
				codeStr = strings.TrimSpace(codeStr)
				code, _ := strconv.Atoi(codeStr)
				stderrCh <- readResult{lines: lines, exitCode: code}
				return
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
		stderrCh <- readResult{lines: lines, exitCode: -1, err: scanner.Err()}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var stdoutLines, stderrLines []string
	exitCode := 0
	completed := 0

	for completed < 2 {
		select {
		case res := <-stdoutCh:
			stdoutLines = res.lines
			if res.err != nil {
				bs.alive = false
			}
			completed++
		case res := <-stderrCh:
			stderrLines = res.lines
			if res.exitCode >= 0 {
				exitCode = res.exitCode
			} else {
				bs.alive = false
			}
			completed++
		case <-timer.C:
			// Timeout: kill the process group so child processes are also killed.
			if bs.cmd != nil && bs.cmd.Process != nil {
				if err := syscall.Kill(-bs.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					_ = err
				}
				bs.cmd.Wait() //nolint:errcheck
			}
			bs.alive = false

			// Drain reader goroutines so they don't leak
			drainTimeout := time.After(2 * time.Second)
			for completed < 2 {
				select {
				case <-stdoutCh:
					completed++
				case <-stderrCh:
					completed++
				case <-drainTimeout:
					completed = 2
				}
			}

			duration := time.Since(start)
			return &agent.ExecResponse{
				Stdout:     strings.Join(stdoutLines, "\n"),
				Stderr:     "command timed out",
				ExitCode:   137,
				DurationMs: duration.Milliseconds(),
			}, nil
		}
	}

	duration := time.Since(start)

	stderrStr := strings.Join(stderrLines, "\n")
	if stateReset {
		resetMsg := "[session state was reset — previous bash process exited]"
		if stderrStr != "" {
			stderrStr = resetMsg + "\n" + stderrStr
		} else {
			stderrStr = resetMsg
		}
	}

	return &agent.ExecResponse{
		Stdout:     strings.Join(stdoutLines, "\n"),
		Stderr:     stderrStr,
		ExitCode:   exitCode,
		DurationMs: duration.Milliseconds(),
	}, nil
}
