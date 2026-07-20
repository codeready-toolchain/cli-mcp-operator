package sandbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSession(t *testing.T) *BashSession {
	t.Helper()
	bs, err := NewBashSession(NewDefaultBashConfig())
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := bs.Close(); err != nil {
			t.Errorf("BashSession.Close: %v", err)
		}
	})
	return bs
}

func TestNewBashSession(t *testing.T) {
	bs := newTestSession(t)
	assert.True(t, bs.IsAlive())
}

func TestExecuteSimpleCommand(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("echo hello", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "hello", res.Stdout)
	assert.Equal(t, 0, res.ExitCode)
	assert.GreaterOrEqual(t, res.Duration, time.Duration(0))
}

func TestExecuteMultilineOutput(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("echo line1; echo line2; echo line3", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "line1\nline2\nline3", res.Stdout)
	assert.Equal(t, 0, res.ExitCode)
}

func TestExitCodes(t *testing.T) {
	bs := newTestSession(t)

	tests := []struct {
		name     string
		command  string
		wantCode int
	}{
		{"success", "true", 0},
		{"failure", "false", 1},
		{"exit 42 in subshell", "(exit 42)", 42},
		{"command not found", "nonexistent_command_xyz 2>/dev/null; echo $? >&2; false", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := bs.Execute(tt.command, DefaultTimeout)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCode, res.ExitCode)
		})
	}
}

func TestEnvironmentPersistence(t *testing.T) {
	bs := newTestSession(t)

	_, err := bs.Execute("export MY_TEST_VAR=hello_world", DefaultTimeout)
	require.NoError(t, err)

	res, err := bs.Execute("echo $MY_TEST_VAR", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "hello_world", res.Stdout)
}

func TestWorkingDirectoryPersistence(t *testing.T) {
	bs := newTestSession(t)

	_, err := bs.Execute("cd /tmp", DefaultTimeout)
	require.NoError(t, err)

	res, err := bs.Execute("pwd", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "/tmp", res.Stdout)
}

func TestStderrCapture(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("echo error_msg >&2", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "error_msg", res.Stderr)
}

func TestTimeout(t *testing.T) {
	bs := newTestSession(t)

	t.Run("returns 137 and timed out message", func(t *testing.T) {
		res, err := bs.Execute("sleep 30", 1*time.Second)
		require.NoError(t, err)
		assert.Equal(t, 137, res.ExitCode)
		assert.Contains(t, res.Stderr, "command timed out")
	})

	t.Run("session survives after timeout", func(t *testing.T) {
		res, err := bs.Execute("echo alive_after_timeout", DefaultTimeout)
		require.NoError(t, err)
		assert.Equal(t, "alive_after_timeout", res.Stdout)
		assert.Equal(t, 0, res.ExitCode)
	})
}

func TestTimeoutClamping(t *testing.T) {
	bs := newTestSession(t)

	t.Run("zero timeout uses default", func(t *testing.T) {
		res, err := bs.Execute("echo ok", 0)
		require.NoError(t, err)
		assert.Equal(t, "ok", res.Stdout)
	})

	t.Run("negative timeout uses default", func(t *testing.T) {
		res, err := bs.Execute("echo ok", -5*time.Second)
		require.NoError(t, err)
		assert.Equal(t, "ok", res.Stdout)
	})
}

func TestCrashRecovery(t *testing.T) {
	bs := newTestSession(t)

	bs.mu.Lock()
	if err := bs.cmd.Process.Kill(); err != nil {
		bs.mu.Unlock()
		t.Fatalf("failed to kill bash process: %v", err)
	}
	bs.cmd.Wait() //nolint:errcheck
	bs.dead = true
	bs.mu.Unlock()

	assert.False(t, bs.IsAlive(), "should report not alive after crash")

	res, err := bs.Execute("echo recovered", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "recovered", res.Stdout)
	assert.Equal(t, 0, res.ExitCode)
	assert.Contains(t, res.Stderr, "session state was reset")
	assert.True(t, bs.IsAlive(), "should report alive after respawn")
}

func TestSequentialCommands(t *testing.T) {
	bs := newTestSession(t)

	for range 5 {
		res, err := bs.Execute("echo ok", DefaultTimeout)
		require.NoError(t, err)
		assert.Equal(t, "ok", res.Stdout)
		assert.Equal(t, 0, res.ExitCode)
	}
}

func TestPipesAndRedirects(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("echo 'a b c' | tr ' ' '\\n' | sort -r", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "c\nb\na", res.Stdout)
	assert.Equal(t, 0, res.ExitCode)
}

func TestEmptyCommand(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
}

func TestMixedStdoutStderr(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("echo out1; echo err1 >&2; echo out2", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "out1\nout2", res.Stdout)
	assert.Contains(t, res.Stderr, "err1")
	assert.Equal(t, 0, res.ExitCode)
}

func TestCloseSession(t *testing.T) {
	bs, err := NewBashSession(NewDefaultBashConfig())
	require.NoError(t, err)

	assert.True(t, bs.IsAlive())
	require.NoError(t, bs.Close())
	assert.False(t, bs.IsAlive())
}

func TestCloseIsIdempotent(t *testing.T) {
	bs, err := NewBashSession(NewDefaultBashConfig())
	require.NoError(t, err)

	require.NoError(t, bs.Close())
	require.NoError(t, bs.Close())
	assert.False(t, bs.IsAlive())
}

func TestDelimiterInOutput(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("echo '__SANDBOX_fake__'", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "__SANDBOX_fake__", res.Stdout)
	assert.Equal(t, 0, res.ExitCode)
}

func TestStdinIsolation(t *testing.T) {
	bs := newTestSession(t)

	t.Run("cat without args does not hang", func(t *testing.T) {
		res, err := bs.Execute("cat </dev/null; echo done", 5*time.Second)
		require.NoError(t, err)
		assert.Contains(t, res.Stdout, "done")
		assert.Equal(t, 0, res.ExitCode)
	})

	t.Run("read builtin gets empty input", func(t *testing.T) {
		res, err := bs.Execute("read -t 1 line; echo \"got: '$line'\"", 5*time.Second)
		require.NoError(t, err)
		assert.Contains(t, res.Stdout, "got:")
	})

	t.Run("session survives after stdin-consuming commands", func(t *testing.T) {
		res, err := bs.Execute("echo still_alive", DefaultTimeout)
		require.NoError(t, err)
		assert.Equal(t, "still_alive", res.Stdout)
	})
}

func TestIdleCrashRecovery(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("echo before_crash", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "before_crash", res.Stdout)

	bs.mu.Lock()
	if killErr := bs.cmd.Process.Kill(); killErr != nil {
		bs.mu.Unlock()
		t.Fatalf("failed to kill bash: %v", killErr)
	}
	bs.cmd.Wait() //nolint:errcheck
	bs.mu.Unlock()

	res, err = bs.Execute("echo after_crash", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "after_crash", res.Stdout)
	assert.Contains(t, res.Stderr, "session state was reset")
}

func TestTimeoutPreservesPartialOutput(t *testing.T) {
	bs := newTestSession(t)

	res, err := bs.Execute("echo partial_line; sleep 30", 1*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 137, res.ExitCode)
	assert.Contains(t, res.Stderr, "command timed out")
	assert.Contains(t, res.Stdout, "partial_line")
}
