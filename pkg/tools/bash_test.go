package tools

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockExecutor struct {
	execFn func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error)
}

func (m *mockExecutor) ExecuteCommand(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
	return m.execFn(ctx, sessionID, command, timeoutSec)
}

func intPtr(i int) *int { return &i }

func newRequestWithSessionID(sessionID string) *mcp.CallToolRequest {
	h := http.Header{}
	h.Set("X-Session-ID", sessionID)
	return &mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{Header: h},
	}
}

func TestBashToolHandle(t *testing.T) {
	t.Run("missing session header — nil Extra", func(t *testing.T) {
		// given
		executor := &mockExecutor{execFn: func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
			t.Fatal("executor should not be called")
			return nil, nil
		}}
		tool := NewBashTool(executor)
		req := &mcp.CallToolRequest{Extra: nil}

		// when
		_, _, err := tool.handle(context.Background(), req, BashInput{Command: "echo hi"})

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing X-Session-ID header")
	})

	t.Run("missing session header — empty value", func(t *testing.T) {
		// given
		executor := &mockExecutor{execFn: func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
			t.Fatal("executor should not be called")
			return nil, nil
		}}
		tool := NewBashTool(executor)
		req := &mcp.CallToolRequest{
			Extra: &mcp.RequestExtra{Header: http.Header{}},
		}

		// when
		_, _, err := tool.handle(context.Background(), req, BashInput{Command: "echo hi"})

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing X-Session-ID header")
	})

	t.Run("empty command", func(t *testing.T) {
		// given
		executor := &mockExecutor{execFn: func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
			t.Fatal("executor should not be called")
			return nil, nil
		}}
		tool := NewBashTool(executor)
		req := newRequestWithSessionID("inv-123")

		// when
		_, _, err := tool.handle(context.Background(), req, BashInput{Command: ""})

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "command is required")
	})

	t.Run("successful command — exit 0", func(t *testing.T) {
		// given
		var gotSessionID, gotCommand string
		var gotTimeout int
		executor := &mockExecutor{execFn: func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
			gotSessionID = sessionID
			gotCommand = command
			gotTimeout = timeoutSec
			return &agent.ExecResponse{
				Stdout:     "hello",
				Stderr:     "",
				ExitCode:   0,
				DurationMs: 42,
			}, nil
		}}
		tool := NewBashTool(executor)
		req := newRequestWithSessionID("inv-abc")

		// when
		result, output, err := tool.handle(context.Background(), req, BashInput{Command: "echo hello", Timeout: intPtr(30)})

		// then
		require.NoError(t, err)
		assert.Nil(t, result)
		assert.Equal(t, "hello", output.Stdout)
		assert.Equal(t, "", output.Stderr)
		assert.Equal(t, 0, output.ExitCode)
		assert.Equal(t, int64(42), output.DurationMs)
		assert.Equal(t, "inv-abc", gotSessionID)
		assert.Equal(t, "echo hello", gotCommand)
		assert.Equal(t, 30, gotTimeout)
	})

	t.Run("non-zero exit code — IsError true with output", func(t *testing.T) {
		// given
		executor := &mockExecutor{execFn: func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
			return &agent.ExecResponse{
				Stdout:     "",
				Stderr:     "not found",
				ExitCode:   1,
				DurationMs: 5,
			}, nil
		}}
		tool := NewBashTool(executor)
		req := newRequestWithSessionID("inv-def")

		// when
		result, output, err := tool.handle(context.Background(), req, BashInput{Command: "oc get pod missing"})

		// then
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.IsError)
		assert.Equal(t, "not found", output.Stderr)
		assert.Equal(t, 1, output.ExitCode)
		assert.Equal(t, int64(5), output.DurationMs)
	})

	t.Run("executor error — infrastructure failure", func(t *testing.T) {
		// given
		executor := &mockExecutor{execFn: func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
			return nil, fmt.Errorf("resolve pod: connection refused")
		}}
		tool := NewBashTool(executor)
		req := newRequestWithSessionID("inv-ghi")

		// when
		_, _, err := tool.handle(context.Background(), req, BashInput{Command: "ls"})

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sandbox exec failed")
		assert.Contains(t, err.Error(), "connection refused")
	})

	t.Run("session ID forwarded unchanged", func(t *testing.T) {
		// given
		var gotSessionID string
		executor := &mockExecutor{execFn: func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
			gotSessionID = sessionID
			return &agent.ExecResponse{ExitCode: 0}, nil
		}}
		tool := NewBashTool(executor)
		req := newRequestWithSessionID("investigation-xyz-789")

		// when
		_, _, err := tool.handle(context.Background(), req, BashInput{Command: "pwd"})

		// then
		require.NoError(t, err)
		assert.Equal(t, "investigation-xyz-789", gotSessionID)
	})
}

func TestClampTimeout(t *testing.T) {
	tests := []struct {
		name     string
		input    *int
		expected int
	}{
		{"nil defaults to 60", nil, 60},
		{"zero defaults to 60", intPtr(0), 60},
		{"negative defaults to 60", intPtr(-5), 60},
		{"within range passes through", intPtr(120), 120},
		{"max boundary passes through", intPtr(300), 300},
		{"above max clamped to 300", intPtr(500), 300},
		{"one is valid", intPtr(1), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// when
			result := clampTimeout(tt.input)

			// then
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRegisterWith(t *testing.T) {
	// given
	executor := &mockExecutor{execFn: func(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
		return &agent.ExecResponse{ExitCode: 0}, nil
	}}
	tool := NewBashTool(executor)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1"}, nil)

	// when
	tool.RegisterWith(server)

	// then
	assert.Equal(t, "bash", tool.Tool().Name)
}
