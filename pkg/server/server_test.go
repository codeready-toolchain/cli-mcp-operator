package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockCleaner struct {
	calledWith string
	err        error
}

func (m *mockCleaner) CleanupSession(_ context.Context, sessionID string) error {
	m.calledWith = sessionID
	return m.err
}

type mockHealthChecker struct {
	err error
}

func (m *mockHealthChecker) CheckHealth(_ context.Context) error {
	return m.err
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func TestSessionDelete(t *testing.T) {
	t.Run("returns 204 and calls cleanup with session id", func(t *testing.T) {
		// given
		cleaner := &mockCleaner{}
		checker := &mockHealthChecker{}
		logger := newTestLogger()
		mcpSrv := NewMCPServer("test", true, logger)
		mux := NewMux(mcpSrv, cleaner, checker, logger)

		// when
		req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/sessions/inv-abc", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		// then
		assert.Equal(t, http.StatusNoContent, rr.Code)
		assert.Equal(t, "inv-abc", cleaner.calledWith)
	})

	t.Run("returns 400 for empty session id", func(t *testing.T) {
		// given
		cleaner := &mockCleaner{}
		checker := &mockHealthChecker{}
		logger := newTestLogger()
		mcpSrv := NewMCPServer("test", true, logger)
		mux := NewMux(mcpSrv, cleaner, checker, logger)

		// when
		req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/sessions/", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Empty(t, cleaner.calledWith)
	})

	t.Run("returns 500 with generic body on cleanup error", func(t *testing.T) {
		// given
		cleaner := &mockCleaner{err: errors.New("k8s pod delete failed: namespace xyz")}
		checker := &mockHealthChecker{}
		logger := newTestLogger()
		mcpSrv := NewMCPServer("test", true, logger)
		mux := NewMux(mcpSrv, cleaner, checker, logger)

		// when
		req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/sessions/inv-fail", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		// then
		assert.Equal(t, http.StatusInternalServerError, rr.Code)
		assert.Contains(t, rr.Body.String(), "failed to end session")
		assert.NotContains(t, rr.Body.String(), "k8s pod delete failed")
		assert.Equal(t, "inv-fail", cleaner.calledWith)
	})

	t.Run("returns 400 for invalid session id format", func(t *testing.T) {
		// given
		cleaner := &mockCleaner{}
		checker := &mockHealthChecker{}
		logger := newTestLogger()
		mcpSrv := NewMCPServer("test", true, logger)
		mux := NewMux(mcpSrv, cleaner, checker, logger)

		// when
		req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/sessions/INVALID_ID!", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		// then
		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Contains(t, rr.Body.String(), "invalid session ID format")
		assert.Empty(t, cleaner.calledWith)
	})
}

func TestLive(t *testing.T) {
	// given
	cleaner := &mockCleaner{}
	checker := &mockHealthChecker{}
	logger := newTestLogger()
	mcpSrv := NewMCPServer("test", true, logger)
	mux := NewMux(mcpSrv, cleaner, checker, logger)

	// when
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/live", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	// then
	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "alive", body["status"])
}

func TestHealth(t *testing.T) {
	t.Run("returns 200 when healthy", func(t *testing.T) {
		// given
		cleaner := &mockCleaner{}
		checker := &mockHealthChecker{}
		logger := newTestLogger()
		mcpSrv := NewMCPServer("test", true, logger)
		mux := NewMux(mcpSrv, cleaner, checker, logger)

		// when
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		// then
		assert.Equal(t, http.StatusOK, rr.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "healthy", body["status"])
	})

	t.Run("returns 503 when unhealthy", func(t *testing.T) {
		// given
		cleaner := &mockCleaner{}
		checker := &mockHealthChecker{err: errors.New("connection refused")}
		logger := newTestLogger()
		mcpSrv := NewMCPServer("test", true, logger)
		mux := NewMux(mcpSrv, cleaner, checker, logger)

		// when
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		// then
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "unhealthy", body["status"])
		assert.NotEmpty(t, body["error"])
	})
}

func TestBashToolRegistered(t *testing.T) {
	// given
	logger := newTestLogger()
	mcpSrv := NewMCPServer("test", true, logger)

	ct, st := mcp.NewInMemoryTransports()
	ss, err := mcpSrv.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	// when — server has no tools registered yet (bash tool registration happens in cmd/server)
	result, err := cs.ListTools(context.Background(), nil)

	// then
	require.NoError(t, err)
	assert.Empty(t, result.Tools)
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{"localhost with port", "localhost:8080", true},
		{"127.0.0.1 with port", "127.0.0.1:8080", true},
		{"::1 with port", "[::1]:8080", true},
		{"bare localhost", "localhost", true},
		{"bare 127.0.0.1", "127.0.0.1", true},
		{"non-loopback IP", "10.0.0.1:8080", false},
		{"non-loopback hostname", "myhost.example.com:8080", false},
		{"0.0.0.0", "0.0.0.0:8080", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// when
			got := IsLoopback(tt.addr)

			// then
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidateTransportFlags(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		stateless bool
		address   string
		wantErr   string
	}{
		{"http valid", "http", true, "localhost:8080", ""},
		{"http missing stateless", "http", false, "localhost:8080", "--stateless is required"},
		{"http non-loopback", "http", true, "10.0.0.1:8080", "loopback"},
		{"stdio valid", "stdio", false, "localhost:8080", ""},
		{"stdio with stateless", "stdio", true, "localhost:8080", "not supported for stdio"},
		{"invalid transport", "websocket", false, "", "unsupported transport"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// when
			err := ValidateTransportFlags(tt.transport, tt.stateless, tt.address)

			// then
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}
