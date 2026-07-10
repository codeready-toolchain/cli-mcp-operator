package agent

import "fmt"

const maxErrorBodySize = 512

// NetworkError is returned when an HTTP request fails at the transport layer
// (connection refused, timeout, DNS failure, network unreachable).
type NetworkError struct {
	Op  string // "execute", "assign", "health_check"
	URL string
	Err error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Op, e.URL, e.Err)
}

func (e *NetworkError) Unwrap() error {
	return e.Err
}

// StatusError is returned when the agent responds with a non-200 HTTP status.
// It is a terminal error — there is no wrapped cause.
type StatusError struct {
	Op         string
	URL        string
	StatusCode int
	Body       string // first maxErrorBodySize bytes of the response body
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: unexpected status %d", e.Op, e.URL, e.StatusCode)
}

// DecodeError is returned when a successful (200) response body cannot be
// decoded as JSON, or when the body exceeds the configured size limit.
type DecodeError struct {
	Op         string
	URL        string
	StatusCode int
	Err        error
	Body       string // first maxErrorBodySize bytes of the raw body
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Op, e.URL, e.Err)
}

func (e *DecodeError) Unwrap() error {
	return e.Err
}
