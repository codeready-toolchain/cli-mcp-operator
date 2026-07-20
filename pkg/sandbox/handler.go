package sandbox

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/agent"
)

// AgentState manages the unassigned/assigned lifecycle of the sandbox agent.
// Warm-pool pods start unassigned; on-demand pods start assigned (token from env).
type AgentState struct {
	mu       sync.Mutex
	assigned bool
	token    string
}

// NewAgentState creates an AgentState. If token is non-empty the agent starts
// in assigned mode; otherwise it starts unassigned (warm pool).
func NewAgentState(token string) *AgentState {
	return &AgentState{
		assigned: token != "",
		token:    token,
	}
}

// IsAssigned reports whether the agent has been assigned to a session.
func (s *AgentState) IsAssigned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.assigned
}

// GetToken returns the current bearer token (empty if unassigned).
func (s *AgentState) GetToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// Assign transitions the agent from unassigned to assigned with the given
// token. Returns an error if already assigned.
func (s *AgentState) Assign(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.assigned {
		return errAlreadyAssigned
	}
	s.token = token
	s.assigned = true
	return nil
}

var errAlreadyAssigned = &alreadyAssignedError{}

type alreadyAssignedError struct{}

func (e *alreadyAssignedError) Error() string { return "already assigned" }

// Handler groups the shared dependencies for the agent's three HTTP endpoints.
type Handler struct {
	session *BashSession
	state   *AgentState
}

// NewHandler creates a Handler wired to the given bash session and agent state.
func NewHandler(session *BashSession, state *AgentState) *Handler {
	return &Handler{session: session, state: state}
}

// ErrorResponse is the JSON envelope for all error responses.
type ErrorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(ErrorResponse{Error: msg}); err != nil {
		return
	}
}

// HandleHealth is an unauthenticated readiness probe.
// Returns 200 if bash is alive, 503 if not.
func (h *Handler) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.session.IsAlive() {
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
			return
		}
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"}); err != nil {
		return
	}
}

// HandleExec executes a command in the persistent bash session.
// Requires the agent to be assigned and a valid bearer token.
func (h *Handler) HandleExec(w http.ResponseWriter, r *http.Request) {
	if !h.state.IsAssigned() {
		writeError(w, http.StatusServiceUnavailable, "agent not assigned")
		return
	}

	provided, ok := extractBearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid authorization header")
		return
	}

	if !hmac.Equal([]byte(provided), []byte(h.state.GetToken())) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req agent.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}

	timeout := time.Duration(req.Timeout) * time.Second

	result, err := h.session.Execute(req.Command, timeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := agent.ExecResponse{
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		ExitCode:   result.ExitCode,
		DurationMs: result.Duration.Milliseconds(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// HandleAssign delivers an auth token to a warm-pool pod, transitioning
// it from unassigned to assigned. Can only be called once (409 on repeat).
func (h *Handler) HandleAssign(w http.ResponseWriter, r *http.Request) {
	var req agent.AssignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	if err := h.state.Assign(req.Token); err != nil {
		writeError(w, http.StatusConflict, "already assigned")
		return
	}

	w.WriteHeader(http.StatusOK)
}

func extractBearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	return auth[len(prefix):], true
}
