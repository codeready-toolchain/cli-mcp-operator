package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/version"
	"github.com/codeready-toolchain/mcp-common/pkg/middleware"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// SessionCleaner abstracts session cleanup for testability.
type SessionCleaner interface {
	CleanupSession(ctx context.Context, sessionID string) error
}

// HealthChecker abstracts the Kubernetes API reachability check.
type HealthChecker interface {
	CheckHealth(ctx context.Context) error
}

// NewMCPServer constructs an mcp.Server with mcp-common middleware.
func NewMCPServer(name string, stateless bool, logger *slog.Logger) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: name, Version: version.Commit},
		&mcp.ServerOptions{
			Capabilities: &mcp.ServerCapabilities{
				Tools: &mcp.ToolCapabilities{ListChanged: !stateless},
			},
			Logger: logger,
		})
	srv.AddReceivingMiddleware(middleware.NewMetricsMiddleware(name, logger))
	srv.AddReceivingMiddleware(middleware.NewLoggingMiddleware(logger))
	return srv
}

// NewMux builds the HTTP mux with all operational endpoints.
func NewMux(mcpServer *mcp.Server, cleaner SessionCleaner, checker HealthChecker, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		DisableLocalhostProtection: true,
	}))

	mux.Handle("/metrics", promhttp.Handler())

	mux.HandleFunc("/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		if err := checker.CheckHealth(checkCtx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "unhealthy",
				"error":  err.Error(),
				"time":   time.Now().Format(time.RFC3339),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "healthy",
			"time":   time.Now().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("DELETE /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if sessionID == "" {
			http.Error(w, "missing session ID", http.StatusBadRequest)
			return
		}
		if err := session.ValidateSessionID(sessionID); err != nil {
			http.Error(w, "invalid session ID format", http.StatusBadRequest)
			return
		}
		if err := cleaner.CleanupSession(r.Context(), sessionID); err != nil {
			logger.Error("session cleanup failed", "session_id", sessionID, "error", err)
			http.Error(w, "failed to end session", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("DELETE /sessions/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing session ID", http.StatusBadRequest)
	})

	return mux
}

// IsLoopback returns true if the host part of addr is a loopback address.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateTransportFlags checks stateless/transport/address consistency.
func ValidateTransportFlags(transport string, stateless bool, address string) error {
	switch transport {
	case "http":
		if !stateless {
			return fmt.Errorf("--stateless is required for HTTP transport")
		}
		if !IsLoopback(address) {
			return fmt.Errorf("HTTP transport requires a loopback --address (got %q); non-loopback addresses are not allowed without an authentication boundary", address)
		}
	case "stdio":
		if stateless {
			return fmt.Errorf("--stateless is not supported for stdio transport")
		}
	default:
		return fmt.Errorf("unsupported transport %q (use \"stdio\" or \"http\")", transport)
	}
	return nil
}
