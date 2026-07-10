package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, ts *httptest.Server, opts ...Option) (*AgentClient, string) {
	t.Helper()
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	host, portStr, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	allOpts := append([]Option{WithHTTPClient(ts.Client()), WithPort(port)}, opts...)
	return NewAgentClient(allOpts...), host
}

func TestExecute(t *testing.T) {
	t.Run("successful execution", func(t *testing.T) {
		// given
		expected := ExecResponse{
			Stdout:     "hello",
			Stderr:     "",
			ExitCode:   0,
			DurationMs: 12,
		}
		var gotMethod, gotPath, gotAuth, gotCT string
		var gotReq ExecRequest
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotCT = r.Header.Get("Content-Type")
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(expected))
		}))
		t.Cleanup(ts.Close)
		client, host := newTestClient(t, ts)

		// when
		resp, err := client.Execute(t.Context(), host, "tok-123", ExecRequest{Command: "echo hello", Timeout: 30})

		// then
		require.NoError(t, err)
		assert.Equal(t, &expected, resp)
		assert.Equal(t, http.MethodPost, gotMethod)
		assert.Equal(t, "/exec", gotPath)
		assert.Equal(t, "Bearer tok-123", gotAuth)
		assert.Equal(t, "application/json", gotCT)
		assert.Equal(t, "echo hello", gotReq.Command)
		assert.Equal(t, 30, gotReq.Timeout)
	})

	t.Run("non-200 returns StatusError", func(t *testing.T) {
		tests := []struct {
			name   string
			status int
			body   string
		}{
			{"unauthorized", http.StatusUnauthorized, "unauthorized"},
			{"server error", http.StatusInternalServerError, "boom"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// given
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, tt.body, tt.status)
				}))
				t.Cleanup(ts.Close)
				client, host := newTestClient(t, ts)

				// when
				resp, err := client.Execute(t.Context(), host, "tok", ExecRequest{Command: "ls"})

				// then
				require.Nil(t, resp)
				var statusErr *StatusError
				require.ErrorAs(t, err, &statusErr)
				assert.Equal(t, "execute", statusErr.Op)
				assert.Equal(t, tt.status, statusErr.StatusCode)
				assert.Contains(t, statusErr.Body, tt.body)
				assert.Contains(t, statusErr.URL, "/exec")
			})
		}
	})

	t.Run("malformed JSON returns DecodeError", func(t *testing.T) {
		// given
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not-json"))
		}))
		t.Cleanup(ts.Close)
		client, host := newTestClient(t, ts)

		// when
		resp, err := client.Execute(t.Context(), host, "tok", ExecRequest{Command: "ls"})

		// then
		require.Nil(t, resp)
		var decodeErr *DecodeError
		require.ErrorAs(t, err, &decodeErr)
		assert.Equal(t, "execute", decodeErr.Op)
		assert.Equal(t, http.StatusOK, decodeErr.StatusCode)
		assert.Equal(t, "not-json", decodeErr.Body)
		assert.NotNil(t, decodeErr.Err)
	})

	t.Run("connection refused returns NetworkError", func(t *testing.T) {
		// given — port 1 is privileged and almost never listening
		client := NewAgentClient(WithPort(1), WithTimeout(2*time.Second))

		// when
		resp, err := client.Execute(t.Context(), "127.0.0.1", "tok", ExecRequest{Command: "ls"})

		// then
		require.Nil(t, resp)
		var netErr *NetworkError
		require.ErrorAs(t, err, &netErr)
		assert.Equal(t, "execute", netErr.Op)
		assert.Contains(t, netErr.URL, "127.0.0.1:1/exec")
		assert.NotNil(t, netErr.Err)
	})

	t.Run("context deadline returns NetworkError wrapping DeadlineExceeded", func(t *testing.T) {
		// given
		release := make(chan struct{})
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-release
		}))
		t.Cleanup(func() {
			close(release)
			ts.Close()
		})
		client, host := newTestClient(t, ts)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		t.Cleanup(cancel)

		// when
		resp, err := client.Execute(ctx, host, "tok", ExecRequest{Command: "ls"})

		// then
		require.Nil(t, resp)
		var netErr *NetworkError
		require.ErrorAs(t, err, &netErr)
		assert.Equal(t, "execute", netErr.Op)
		assert.True(t, errors.Is(err, context.DeadlineExceeded))
	})

	t.Run("context cancellation returns NetworkError wrapping Canceled", func(t *testing.T) {
		// given
		started := make(chan struct{})
		release := make(chan struct{})
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
		}))
		t.Cleanup(func() {
			close(release)
			ts.Close()
		})
		client, host := newTestClient(t, ts)
		ctx, cancel := context.WithCancel(t.Context())

		errCh := make(chan error, 1)
		go func() {
			_, err := client.Execute(ctx, host, "tok", ExecRequest{Command: "ls"})
			errCh <- err
		}()

		// when
		<-started
		cancel()
		err := <-errCh

		// then
		var netErr *NetworkError
		require.ErrorAs(t, err, &netErr)
		assert.True(t, errors.Is(err, context.Canceled))
	})

	t.Run("response exceeding max size returns DecodeError", func(t *testing.T) {
		// given
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"stdout":"` + strings.Repeat("x", 100) + `","stderr":"","exit_code":0,"duration_ms":1}`))
		}))
		t.Cleanup(ts.Close)
		client, host := newTestClient(t, ts, WithMaxResponseSize(32))

		// when
		resp, err := client.Execute(t.Context(), host, "tok", ExecRequest{Command: "ls"})

		// then
		require.Nil(t, resp)
		var decodeErr *DecodeError
		require.ErrorAs(t, err, &decodeErr)
		assert.Equal(t, http.StatusOK, decodeErr.StatusCode)
		assert.Contains(t, decodeErr.Err.Error(), "exceeds maximum size")
	})
}

func TestAssign(t *testing.T) {
	t.Run("successful assignment", func(t *testing.T) {
		// given
		var gotMethod, gotPath, gotAuth, gotCT string
		var gotReq AssignRequest
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotCT = r.Header.Get("Content-Type")
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)
		client, host := newTestClient(t, ts)

		// when
		err := client.Assign(t.Context(), host, AssignRequest{Token: "hmac-tok"})

		// then
		require.NoError(t, err)
		assert.Equal(t, http.MethodPost, gotMethod)
		assert.Equal(t, "/assign", gotPath)
		assert.Equal(t, "application/json", gotCT)
		assert.Empty(t, gotAuth)
		assert.Equal(t, "hmac-tok", gotReq.Token)
	})

	t.Run("409 conflict returns StatusError", func(t *testing.T) {
		// given
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "already assigned", http.StatusConflict)
		}))
		t.Cleanup(ts.Close)
		client, host := newTestClient(t, ts)

		// when
		err := client.Assign(t.Context(), host, AssignRequest{Token: "tok"})

		// then
		var statusErr *StatusError
		require.ErrorAs(t, err, &statusErr)
		assert.Equal(t, "assign", statusErr.Op)
		assert.Equal(t, http.StatusConflict, statusErr.StatusCode)
	})

	t.Run("transport failure returns NetworkError", func(t *testing.T) {
		// given
		client := NewAgentClient(WithPort(1), WithTimeout(2*time.Second))

		// when
		err := client.Assign(t.Context(), "127.0.0.1", AssignRequest{Token: "tok"})

		// then
		var netErr *NetworkError
		require.ErrorAs(t, err, &netErr)
		assert.Equal(t, "assign", netErr.Op)
	})
}

func TestHealthCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		// given
		var gotMethod, gotPath, gotAuth string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"healthy"}`)
		}))
		t.Cleanup(ts.Close)
		client, host := newTestClient(t, ts)

		// when
		err := client.HealthCheck(t.Context(), host)

		// then
		require.NoError(t, err)
		assert.Equal(t, http.MethodGet, gotMethod)
		assert.Equal(t, "/health", gotPath)
		assert.Empty(t, gotAuth)
	})

	t.Run("unhealthy returns StatusError", func(t *testing.T) {
		// given
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "bash dead", http.StatusServiceUnavailable)
		}))
		t.Cleanup(ts.Close)
		client, host := newTestClient(t, ts)

		// when
		err := client.HealthCheck(t.Context(), host)

		// then
		var statusErr *StatusError
		require.ErrorAs(t, err, &statusErr)
		assert.Equal(t, "health_check", statusErr.Op)
		assert.Equal(t, http.StatusServiceUnavailable, statusErr.StatusCode)
	})

	t.Run("unreachable returns NetworkError", func(t *testing.T) {
		// given
		client := NewAgentClient(WithPort(1), WithTimeout(2*time.Second))

		// when
		err := client.HealthCheck(t.Context(), "127.0.0.1")

		// then
		var netErr *NetworkError
		require.ErrorAs(t, err, &netErr)
		assert.Equal(t, "health_check", netErr.Op)
	})
}

func TestNewAgentClientOptions(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		// when
		c := NewAgentClient()

		// then
		assert.Equal(t, 30*time.Second, c.httpClient.Timeout)
		assert.Equal(t, 8090, c.port)
		assert.Equal(t, DefaultMaxResponseSize, c.maxResponseSize)
		assert.Equal(t, int64(10*1024*1024), DefaultMaxResponseSize)
	})

	t.Run("WithTimeout overrides timeout", func(t *testing.T) {
		// when
		c := NewAgentClient(WithTimeout(10 * time.Second))

		// then
		assert.Equal(t, 10*time.Second, c.httpClient.Timeout)
	})

	t.Run("WithPort overrides port in URL", func(t *testing.T) {
		// given
		c := NewAgentClient(WithPort(9090))

		// when
		got := c.buildURL("10.0.0.1", "/exec")

		// then
		assert.Equal(t, "http://10.0.0.1:9090/exec", got)
	})

	t.Run("WithPort uses JoinHostPort for IPv6", func(t *testing.T) {
		// given
		c := NewAgentClient(WithPort(8090))

		// when
		got := c.buildURL("2001:db8::1", "/health")

		// then
		assert.Equal(t, "http://[2001:db8::1]:8090/health", got)
	})

	t.Run("WithHTTPClient replaces client", func(t *testing.T) {
		// given
		custom := &http.Client{Timeout: 5 * time.Second}

		// when
		c := NewAgentClient(WithHTTPClient(custom))

		// then
		assert.Same(t, custom, c.httpClient)
	})

	t.Run("WithMaxResponseSize overrides default", func(t *testing.T) {
		// when
		c := NewAgentClient(WithMaxResponseSize(1024))

		// then
		assert.Equal(t, int64(1024), c.maxResponseSize)
	})
}

func TestErrorTypes(t *testing.T) {
	t.Run("errors.As works for all types", func(t *testing.T) {
		// given
		netErr := &NetworkError{Op: "execute", URL: "http://x/exec", Err: context.DeadlineExceeded}
		statusErr := &StatusError{Op: "execute", URL: "http://x/exec", StatusCode: 500, Body: "fail"}
		decodeErr := &DecodeError{Op: "execute", URL: "http://x/exec", StatusCode: 200, Err: errors.New("bad json"), Body: "{"}

		// then
		var asNet *NetworkError
		require.ErrorAs(t, netErr, &asNet)
		var asStatus *StatusError
		require.ErrorAs(t, statusErr, &asStatus)
		var asDecode *DecodeError
		require.ErrorAs(t, decodeErr, &asDecode)
	})

	t.Run("NetworkError unwraps for errors.Is", func(t *testing.T) {
		// given
		err := &NetworkError{Op: "execute", URL: "http://x/exec", Err: context.DeadlineExceeded}

		// then
		assert.True(t, errors.Is(err, context.DeadlineExceeded))
		assert.Equal(t, "execute http://x/exec: context deadline exceeded", err.Error())
	})

	t.Run("DecodeError unwraps underlying error", func(t *testing.T) {
		// given
		inner := errors.New("invalid character")
		err := &DecodeError{Op: "execute", URL: "http://x/exec", StatusCode: 200, Err: inner, Body: "x"}

		// then
		assert.True(t, errors.Is(err, inner))
		assert.Equal(t, "execute http://x/exec: invalid character", err.Error())
	})

	t.Run("StatusError has no wrapped cause", func(t *testing.T) {
		// given
		err := &StatusError{Op: "assign", URL: "http://x/assign", StatusCode: 409, Body: "conflict"}

		// then
		assert.Nil(t, errors.Unwrap(err))
		assert.Equal(t, "assign http://x/assign: unexpected status 409", err.Error())
	})

	t.Run("StatusError body truncated to 512 bytes", func(t *testing.T) {
		// given
		large := strings.Repeat("a", 2000)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, large)
		}))
		t.Cleanup(ts.Close)
		client, host := newTestClient(t, ts)

		// when
		_, err := client.Execute(t.Context(), host, "tok", ExecRequest{Command: "ls"})

		// then
		var statusErr *StatusError
		require.ErrorAs(t, err, &statusErr)
		assert.Len(t, statusErr.Body, maxErrorBodySize)
	})
}

func TestExecuteConcurrency(t *testing.T) {
	// given
	var mu sync.Mutex
	seen := map[string]string{} // token -> path pod marker from header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[token] = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ExecResponse{Stdout: token, ExitCode: 0})
	}))
	t.Cleanup(ts.Close)
	client, host := newTestClient(t, ts)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	resps := make([]*ExecResponse, n)

	// when
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := "tok-" + strconv.Itoa(i)
			resps[i], errs[i] = client.Execute(t.Context(), host, token, ExecRequest{Command: "echo"})
		}(i)
	}
	wg.Wait()

	// then
	for i := range n {
		require.NoError(t, errs[i])
		require.NotNil(t, resps[i])
		assert.Equal(t, "tok-"+strconv.Itoa(i), resps[i].Stdout)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, seen, n)
}
