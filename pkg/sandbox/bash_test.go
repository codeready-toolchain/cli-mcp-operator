package sandbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSession(t *testing.T) *BashSession {
	t.Helper()
	bs, err := NewBashSession()
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
	assert.True(t, bs.Alive())
}

func TestExecuteSimpleCommand(t *testing.T) {
	bs := newTestSession(t)

	resp, err := bs.Execute("echo hello", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "hello", resp.Stdout)
	assert.Equal(t, 0, resp.ExitCode)
	assert.GreaterOrEqual(t, resp.DurationMs, int64(0))
}

func TestExecuteMultilineOutput(t *testing.T) {
	bs := newTestSession(t)

	resp, err := bs.Execute("echo line1; echo line2; echo line3", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "line1\nline2\nline3", resp.Stdout)
	assert.Equal(t, 0, resp.ExitCode)
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
			resp, err := bs.Execute(tt.command, DefaultTimeout)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCode, resp.ExitCode)
		})
	}
}

func TestEnvironmentPersistence(t *testing.T) {
	bs := newTestSession(t)

	_, err := bs.Execute("export MY_TEST_VAR=hello_world", DefaultTimeout)
	require.NoError(t, err)

	resp, err := bs.Execute("echo $MY_TEST_VAR", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "hello_world", resp.Stdout)
}

func TestWorkingDirectoryPersistence(t *testing.T) {
	bs := newTestSession(t)

	_, err := bs.Execute("cd /tmp", DefaultTimeout)
	require.NoError(t, err)

	resp, err := bs.Execute("pwd", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "/tmp", resp.Stdout)
}

func TestStderrCapture(t *testing.T) {
	bs := newTestSession(t)

	resp, err := bs.Execute("echo error_msg >&2", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "error_msg", resp.Stderr)
}

func TestTimeout(t *testing.T) {
	bs := newTestSession(t)

	t.Run("returns 137 and timed out message", func(t *testing.T) {
		resp, err := bs.Execute("sleep 30", 1*time.Second)
		require.NoError(t, err)
		assert.Equal(t, 137, resp.ExitCode)
		assert.Equal(t, "command timed out", resp.Stderr)
	})

	t.Run("session survives after timeout", func(t *testing.T) {
		resp, err := bs.Execute("echo alive_after_timeout", DefaultTimeout)
		require.NoError(t, err)
		assert.Equal(t, "alive_after_timeout", resp.Stdout)
		assert.Equal(t, 0, resp.ExitCode)
	})
}

func TestTimeoutClamping(t *testing.T) {
	bs := newTestSession(t)

	t.Run("zero timeout uses default", func(t *testing.T) {
		resp, err := bs.Execute("echo ok", 0)
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Stdout)
	})

	t.Run("negative timeout uses default", func(t *testing.T) {
		resp, err := bs.Execute("echo ok", -5*time.Second)
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Stdout)
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
	bs.alive = false
	bs.mu.Unlock()

	assert.False(t, bs.Alive(), "should report not alive after crash")

	resp, err := bs.Execute("echo recovered", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "recovered", resp.Stdout)
	assert.Equal(t, 0, resp.ExitCode)
	assert.Contains(t, resp.Stderr, "session state was reset")
	assert.True(t, bs.Alive(), "should report alive after respawn")
}

func TestSequentialCommands(t *testing.T) {
	bs := newTestSession(t)

	for range 5 {
		resp, err := bs.Execute("echo ok", DefaultTimeout)
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Stdout)
		assert.Equal(t, 0, resp.ExitCode)
	}
}

func TestPipesAndRedirects(t *testing.T) {
	bs := newTestSession(t)

	resp, err := bs.Execute("echo 'a b c' | tr ' ' '\\n' | sort -r", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "c\nb\na", resp.Stdout)
	assert.Equal(t, 0, resp.ExitCode)
}

func TestEmptyCommand(t *testing.T) {
	bs := newTestSession(t)

	resp, err := bs.Execute("", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, 0, resp.ExitCode)
}

func TestMixedStdoutStderr(t *testing.T) {
	bs := newTestSession(t)

	resp, err := bs.Execute("echo out1; echo err1 >&2; echo out2", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "out1\nout2", resp.Stdout)
	assert.Contains(t, resp.Stderr, "err1")
	assert.Equal(t, 0, resp.ExitCode)
}

func TestCloseSession(t *testing.T) {
	bs, err := NewBashSession()
	require.NoError(t, err)

	assert.True(t, bs.Alive())
	require.NoError(t, bs.Close())
	assert.False(t, bs.Alive())
}

func TestDelimiterInOutput(t *testing.T) {
	bs := newTestSession(t)

	resp, err := bs.Execute("echo '__SANDBOX_fake__'", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "__SANDBOX_fake__", resp.Stdout)
	assert.Equal(t, 0, resp.ExitCode)
}
