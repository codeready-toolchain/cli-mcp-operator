package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newTestHandler(t *testing.T, token string) *Handler {
	t.Helper()
	bs := newTestSession(t)
	state := NewAgentState(token)
	return NewHandler(bs, state)
}

func TestHandleHealth(t *testing.T) {
	t.Run("returns 200 when bash is alive", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		req := httptest.NewRequestWithContext(testContext(t), http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()

		// when
		h.HandleHealth(rec, req)

		// then
		assert.Equal(t, http.StatusOK, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "ok", body["status"])
	})

	t.Run("returns 503 when bash is dead", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		require.NoError(t, h.session.Close())
		req := httptest.NewRequestWithContext(testContext(t), http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()

		// when
		h.HandleHealth(rec, req)

		// then
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "unhealthy", body["status"])
	})
}

func TestHandleExec(t *testing.T) {
	t.Run("rejects when agent not assigned", func(t *testing.T) {
		// given
		h := newTestHandler(t, "")
		body, _ := json.Marshal(agent.ExecRequest{Command: "echo hi"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer anything")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "agent not assigned", resp.Error)
	})

	t.Run("rejects missing authorization header", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		body, _ := json.Marshal(agent.ExecRequest{Command: "echo hi"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "missing or invalid authorization header", resp.Error)
	})

	t.Run("rejects malformed authorization header", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		body, _ := json.Marshal(agent.ExecRequest{Command: "echo hi"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "missing or invalid authorization header", resp.Error)
	})

	t.Run("rejects wrong token", func(t *testing.T) {
		// given
		h := newTestHandler(t, "correct-secret")
		body, _ := json.Marshal(agent.ExecRequest{Command: "echo hi"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer wrong-secret")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "unauthorized", resp.Error)
	})

	t.Run("rejects invalid request body", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader([]byte("not json")))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "invalid request body", resp.Error)
	})

	t.Run("rejects empty command", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		body, _ := json.Marshal(agent.ExecRequest{Command: ""})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "command is required", resp.Error)
	})

	t.Run("rejects oversized request body", func(t *testing.T) {
		// given — valid JSON whose encoded size exceeds maxRequestBody
		h := newTestHandler(t, "secret")
		body, err := json.Marshal(agent.ExecRequest{Command: strings.Repeat("x", maxRequestBody)})
		require.NoError(t, err)
		require.Greater(t, len(body), maxRequestBody)
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "invalid request body", resp.Error)
	})

	t.Run("rejects trailing data after valid JSON", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		body, err := json.Marshal(agent.ExecRequest{Command: "echo ok"})
		require.NoError(t, err)
		body = append(body, []byte(`{"extra":true}`)...)
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("executes command and returns stdout", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		body, _ := json.Marshal(agent.ExecRequest{Command: "echo hello_handler"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusOK, rec.Code)
		var resp agent.ExecResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "hello_handler", resp.Stdout)
		assert.Equal(t, 0, resp.ExitCode)
		assert.GreaterOrEqual(t, resp.DurationMs, int64(0))
	})

	t.Run("returns non-zero exit code from failing command", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		body, _ := json.Marshal(agent.ExecRequest{Command: "(exit 42)"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusOK, rec.Code)
		var resp agent.ExecResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, 42, resp.ExitCode)
	})

	t.Run("captures stderr output", func(t *testing.T) {
		// given
		h := newTestHandler(t, "secret")
		body, _ := json.Marshal(agent.ExecRequest{Command: "echo oops >&2"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()

		// when
		h.HandleExec(rec, req)

		// then
		assert.Equal(t, http.StatusOK, rec.Code)
		var resp agent.ExecResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "oops", resp.Stderr)
	})
}

func TestHandleAssign(t *testing.T) {
	t.Run("assigns token to unassigned agent", func(t *testing.T) {
		// given
		h := newTestHandler(t, "")
		body, _ := json.Marshal(agent.AssignRequest{Token: "new-token"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		// when
		h.HandleAssign(rec, req)

		// then
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, h.state.IsAssigned())
		assert.Equal(t, "new-token", h.state.GetToken())
	})

	t.Run("returns 409 when already assigned", func(t *testing.T) {
		// given
		h := newTestHandler(t, "existing-token")
		body, _ := json.Marshal(agent.AssignRequest{Token: "another-token"})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		// when
		h.HandleAssign(rec, req)

		// then
		assert.Equal(t, http.StatusConflict, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "already assigned", resp.Error)
	})

	t.Run("returns 409 on second assign attempt", func(t *testing.T) {
		// given
		h := newTestHandler(t, "")
		body1, _ := json.Marshal(agent.AssignRequest{Token: "first-token"})
		req1 := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader(body1))
		rec1 := httptest.NewRecorder()
		h.HandleAssign(rec1, req1)
		require.Equal(t, http.StatusOK, rec1.Code)

		body2, _ := json.Marshal(agent.AssignRequest{Token: "second-token"})
		req2 := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader(body2))
		rec2 := httptest.NewRecorder()

		// when
		h.HandleAssign(rec2, req2)

		// then
		assert.Equal(t, http.StatusConflict, rec2.Code)
		assert.Equal(t, "first-token", h.state.GetToken())
	})

	t.Run("rejects invalid request body", func(t *testing.T) {
		// given
		h := newTestHandler(t, "")
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader([]byte("bad")))
		rec := httptest.NewRecorder()

		// when
		h.HandleAssign(rec, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "invalid request body", resp.Error)
	})

	t.Run("rejects oversized request body", func(t *testing.T) {
		// given — valid JSON whose encoded size exceeds maxRequestBody
		h := newTestHandler(t, "")
		body, err := json.Marshal(agent.AssignRequest{Token: strings.Repeat("t", maxRequestBody)})
		require.NoError(t, err)
		require.Greater(t, len(body), maxRequestBody)
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		// when
		h.HandleAssign(rec, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "invalid request body", resp.Error)
		assert.False(t, h.state.IsAssigned())
	})

	t.Run("rejects empty token", func(t *testing.T) {
		// given
		h := newTestHandler(t, "")
		body, _ := json.Marshal(agent.AssignRequest{Token: ""})
		req := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		// when
		h.HandleAssign(rec, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "token is required", resp.Error)
	})
}

func TestAssignThenExec(t *testing.T) {
	t.Run("exec works after assign", func(t *testing.T) {
		// given
		h := newTestHandler(t, "")
		assignBody, _ := json.Marshal(agent.AssignRequest{Token: "dynamic-token"})
		assignReq := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader(assignBody))
		assignRec := httptest.NewRecorder()
		h.HandleAssign(assignRec, assignReq)
		require.Equal(t, http.StatusOK, assignRec.Code)

		execBody, _ := json.Marshal(agent.ExecRequest{Command: "echo from_warm_pool"})
		execReq := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(execBody))
		execReq.Header.Set("Authorization", "Bearer dynamic-token")
		execRec := httptest.NewRecorder()

		// when
		h.HandleExec(execRec, execReq)

		// then
		assert.Equal(t, http.StatusOK, execRec.Code)
		var resp agent.ExecResponse
		require.NoError(t, json.Unmarshal(execRec.Body.Bytes(), &resp))
		assert.Equal(t, "from_warm_pool", resp.Stdout)
		assert.Equal(t, 0, resp.ExitCode)
	})

	t.Run("exec rejected with wrong token after assign", func(t *testing.T) {
		// given
		h := newTestHandler(t, "")
		assignBody, _ := json.Marshal(agent.AssignRequest{Token: "correct-token"})
		assignReq := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/assign", bytes.NewReader(assignBody))
		assignRec := httptest.NewRecorder()
		h.HandleAssign(assignRec, assignReq)
		require.Equal(t, http.StatusOK, assignRec.Code)

		execBody, _ := json.Marshal(agent.ExecRequest{Command: "echo should_fail"})
		execReq := httptest.NewRequestWithContext(testContext(t), http.MethodPost, "/exec", bytes.NewReader(execBody))
		execReq.Header.Set("Authorization", "Bearer wrong-token")
		execRec := httptest.NewRecorder()

		// when
		h.HandleExec(execRec, execReq)

		// then
		assert.Equal(t, http.StatusUnauthorized, execRec.Code)
	})
}
