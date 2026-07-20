package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/sandbox"
	"github.com/codeready-toolchain/cli-mcp-server/pkg/version"
)

const shutdownTimeout = 310 * time.Second // MaxTimeout (300s) + 10s buffer

func main() {
	fmt.Fprintf(os.Stderr, "sandbox-agent %s (built %s)\n", version.Commit, version.BuildTime)

	token := os.Getenv("SANDBOX_AUTH_TOKEN")
	state := sandbox.NewAgentState(token)
	if token != "" {
		log.Println("starting in assigned state")
	} else {
		log.Println("starting in unassigned state (warm pool)")
	}

	session, err := sandbox.NewBashSession(sandbox.NewDefaultBashConfig())
	if err != nil {
		log.Fatalf("failed to create bash session: %v", err)
	}

	handler := sandbox.NewHandler(session, state)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handler.HandleExec)
	mux.HandleFunc("POST /assign", handler.HandleAssign)
	mux.HandleFunc("GET /health", handler.HandleHealth)

	srv := &http.Server{
		Addr:              ":8090",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Println("listening on :8090")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen error: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-ctx.Done()

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	if err := session.Close(); err != nil {
		log.Printf("session close error: %v", err)
	}
	log.Println("shutdown complete")
}
