package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	// DefaultMaxResponseSize is the default cap on successful Execute response bodies (10 MB).
	DefaultMaxResponseSize int64 = 10 * 1024 * 1024

	defaultTimeout = 30 * time.Second
	defaultPort    = 8090
	drainLimit     = 4 * 1024
)

// AgentClient is a typed HTTP client for the sandbox agent API.
// It is safe for concurrent use from multiple goroutines.
//
// Communication uses plain HTTP within the Kubernetes pod network trust boundary:
// NetworkPolicy restricts ingress to sandbox pods, and bearer tokens authenticate
// callers. TLS can be added later as a localized change if needed.
type AgentClient struct {
	httpClient      *http.Client
	port            int
	maxResponseSize int64
}

// Option configures an AgentClient.
type Option func(*AgentClient)

// WithTimeout sets the http.Client.Timeout for all requests.
func WithTimeout(d time.Duration) Option {
	return func(c *AgentClient) {
		client := *c.httpClient
		client.Timeout = d
		c.httpClient = &client
	}
}

// WithHTTPClient replaces the internal HTTP client (useful for testing).
func WithHTTPClient(client *http.Client) Option {
	return func(c *AgentClient) {
		c.httpClient = client
	}
}

// WithPort overrides the default agent port (8090).
func WithPort(port int) Option {
	return func(c *AgentClient) {
		c.port = port
	}
}

// WithMaxResponseSize overrides the maximum response body size for successful Execute responses.
func WithMaxResponseSize(n int64) Option {
	return func(c *AgentClient) {
		c.maxResponseSize = n
	}
}

// NewAgentClient creates an AgentClient with sensible defaults:
// 30s timeout, port 8090, and DefaultMaxResponseSize (10 MB).
func NewAgentClient(opts ...Option) *AgentClient {
	c := &AgentClient{
		httpClient:      &http.Client{Timeout: defaultTimeout},
		port:            defaultPort,
		maxResponseSize: DefaultMaxResponseSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Execute POSTs an ExecRequest to the agent's /exec endpoint with bearer auth.
func (c *AgentClient) Execute(ctx context.Context, podIP, token string, req ExecRequest) (*ExecResponse, error) {
	const op = "execute"
	url := c.buildURL(podIP, "/exec")

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal exec request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create exec request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, classifyDoError(err, op, url)
	}
	defer resp.Body.Close() //nolint:errcheck // body close errors are not actionable

	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{
			Op:         op,
			URL:        url,
			StatusCode: resp.StatusCode,
			Body:       readBodyTruncated(resp.Body, maxErrorBodySize),
		}
	}

	limited := io.LimitReader(resp.Body, c.maxResponseSize+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, &DecodeError{
			Op:         op,
			URL:        url,
			StatusCode: resp.StatusCode,
			Err:        err,
			Body:       truncateString(string(raw), maxErrorBodySize),
		}
	}
	if int64(len(raw)) > c.maxResponseSize {
		return nil, &DecodeError{
			Op:         op,
			URL:        url,
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("response body exceeds maximum size of %d bytes", c.maxResponseSize),
			Body:       truncateString(string(raw), maxErrorBodySize),
		}
	}

	var execResp ExecResponse
	if err := json.Unmarshal(raw, &execResp); err != nil {
		return nil, &DecodeError{
			Op:         op,
			URL:        url,
			StatusCode: resp.StatusCode,
			Err:        err,
			Body:       truncateString(string(raw), maxErrorBodySize),
		}
	}
	return &execResp, nil
}

// Assign POSTs an AssignRequest to the agent's /assign endpoint (no auth).
func (c *AgentClient) Assign(ctx context.Context, podIP string, req AssignRequest) error {
	const op = "assign"
	url := c.buildURL(podIP, "/assign")

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal assign request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create assign request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return classifyDoError(err, op, url)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return &StatusError{
			Op:         op,
			URL:        url,
			StatusCode: resp.StatusCode,
			Body:       readBodyTruncated(resp.Body, maxErrorBodySize),
		}
	}
	drainBody(resp.Body)
	return nil
}

// HealthCheck GETs the agent's /health endpoint (no auth).
func (c *AgentClient) HealthCheck(ctx context.Context, podIP string) error {
	const op = "health_check"
	url := c.buildURL(podIP, "/health")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create health check request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return classifyDoError(err, op, url)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return &StatusError{
			Op:         op,
			URL:        url,
			StatusCode: resp.StatusCode,
			Body:       readBodyTruncated(resp.Body, maxErrorBodySize),
		}
	}
	drainBody(resp.Body)
	return nil
}

func (c *AgentClient) buildURL(podIP, path string) string {
	host := net.JoinHostPort(podIP, strconv.Itoa(c.port))
	return "http://" + host + path
}

func classifyDoError(err error, op, url string) *NetworkError {
	return &NetworkError{Op: op, URL: url, Err: err}
}

func readBodyTruncated(body io.Reader, limit int) string {
	buf := make([]byte, limit)
	n, _ := io.ReadFull(body, buf)
	// ReadFull returns ErrUnexpectedEOF when fewer than limit bytes are available;
	// that is the normal case for short bodies, so ignore the error.
	if n <= 0 {
		return ""
	}
	return string(buf[:n])
}

func truncateString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// drainBody reads up to drainLimit bytes into io.Discard so the underlying
// connection can be reused. The caller is responsible for closing the body.
func drainBody(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimit))
}
