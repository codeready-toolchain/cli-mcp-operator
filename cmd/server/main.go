package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/server"
	"github.com/codeready-toolchain/cli-mcp-server/pkg/session"
	"github.com/codeready-toolchain/cli-mcp-server/pkg/tools"
	"github.com/codeready-toolchain/cli-mcp-server/pkg/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const shutdownTimeout = 310 * time.Second

func main() {
	fmt.Fprintf(os.Stderr, "cli-mcp-server %s (built %s)\n", version.Commit, version.BuildTime)

	var (
		address      string
		transport    string
		stateless    bool
		namespace    string
		sandboxImage string
		kubeconfig   string
		hmacKeyFile  string
		idleTimeout  time.Duration
		warmPoolSize int
	)

	rootCmd := &cobra.Command{
		Use:   "cli-mcp-server",
		Short: "Sandboxed exec environment MCP server for LLM investigation",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runServer(runConfig{
				address:      address,
				transport:    transport,
				stateless:    stateless,
				namespace:    namespace,
				sandboxImage: sandboxImage,
				kubeconfig:   kubeconfig,
				hmacKeyFile:  hmacKeyFile,
				idleTimeout:  idleTimeout,
				warmPoolSize: warmPoolSize,
			})
		},
	}

	rootCmd.Flags().StringVarP(&address, "address", "a", "localhost:8080", "Server address (host:port)")
	rootCmd.Flags().StringVarP(&transport, "transport", "t", "stdio", "Transport (stdio, http)")
	rootCmd.Flags().BoolVar(&stateless, "stateless", false, "Enable stateless mode (required for HTTP)")
	rootCmd.Flags().StringVar(&namespace, "namespace", "tarsy", "Namespace for sandbox pods")
	rootCmd.Flags().StringVar(&sandboxImage, "sandbox-image", "", "Container image for sandbox pods (required)")
	rootCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig for sandbox pods")
	rootCmd.Flags().StringVar(&hmacKeyFile, "hmac-key-file", "", "Path to HMAC shared secret file (required)")
	rootCmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 30*time.Minute, "Idle timeout for sandbox pods")
	rootCmd.Flags().IntVar(&warmPoolSize, "warm-pool-size", 0, "Pre-warmed sandbox pods (0 = disabled)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

type runConfig struct {
	address      string
	transport    string
	stateless    bool
	namespace    string
	sandboxImage string
	kubeconfig   string
	hmacKeyFile  string
	idleTimeout  time.Duration
	warmPoolSize int
}

func runServer(cfg runConfig) error {
	if err := server.ValidateTransportFlags(cfg.transport, cfg.stateless, cfg.address); err != nil {
		return err
	}
	if cfg.sandboxImage == "" {
		return fmt.Errorf("--sandbox-image is required")
	}
	if cfg.idleTimeout <= 0 {
		return fmt.Errorf("--idle-timeout must be greater than zero")
	}
	if cfg.warmPoolSize < 0 {
		return fmt.Errorf("--warm-pool-size must not be negative")
	}
	hmacKey, err := loadHMACKey(cfg.hmacKeyFile)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: true,
	})).With("server", "cli-mcp-server")

	clientset, err := buildClientset(cfg.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	sandboxCfg := session.DefaultConfig()
	sandboxCfg.Image = cfg.sandboxImage
	sandboxCfg.HMACKey = hmacKey
	sandboxCfg.Namespace = cfg.namespace
	sandboxCfg.IdleTimeout = cfg.idleTimeout
	sandboxCfg.WarmPoolSize = cfg.warmPoolSize

	mgr, err := session.NewSessionManager(clientset, sandboxCfg, logger)
	if err != nil {
		return fmt.Errorf("failed to create session manager: %w", err)
	}

	mcpServer := server.NewMCPServer("cli-mcp-server", cfg.stateless, logger)
	tools.NewBashTool(mgr).RegisterWith(mcpServer)

	// Shared context cancelled on SIGTERM/SIGINT — stops background workers for both transports.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	startCleanupLoop(ctx, mgr, logger)
	if cfg.warmPoolSize > 0 {
		mgr.StartPool(ctx)
	}

	switch cfg.transport {
	case "http":
		return serveHTTP(ctx, cancel, cfg.address, mcpServer, mgr, clientset, cfg.namespace, logger)
	case "stdio":
		return serveStdio(ctx, mcpServer, logger)
	default:
		return fmt.Errorf("unsupported transport: %s", cfg.transport)
	}
}

func serveHTTP(ctx context.Context, cancel context.CancelFunc, address string, mcpServer *mcp.Server, mgr *session.SessionManager, clientset kubernetes.Interface, namespace string, logger *slog.Logger) error {
	checker := &k8sHealthChecker{clientset: clientset, namespace: namespace}
	mux := server.NewMux(mcpServer, mgr, checker, logger)

	srv := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // mitigate slowloris; leave ReadTimeout unset for MCP streams
	}
	serverErrChan := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", address, "transport", "http", "stateless", true)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("received shutdown signal, draining")
	case err := <-serverErrChan:
		cancel()
		return fmt.Errorf("HTTP serve failed: %w", err)
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}
	logger.Info("shutdown complete")
	return nil
}

func serveStdio(ctx context.Context, mcpServer *mcp.Server, logger *slog.Logger) error {
	logger.Info("serving on stdio")
	if err := mcpServer.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return fmt.Errorf("stdio transport error: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}

func startCleanupLoop(ctx context.Context, mgr *session.SessionManager, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleaned, err := mgr.CleanupStale(ctx)
				if err != nil {
					logger.Error("stale cleanup failed", "error", err)
				} else if cleaned > 0 {
					logger.Info("cleaned stale sessions", "count", cleaned)
				}
			}
		}
	}()
}

func buildClientset(kubeconfigPath string) (kubernetes.Interface, error) {
	var config *rest.Config
	var err error
	if kubeconfigPath != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

type k8sHealthChecker struct {
	clientset kubernetes.Interface
	namespace string
}

func (c *k8sHealthChecker) CheckHealth(ctx context.Context) error {
	_, err := c.clientset.CoreV1().Namespaces().Get(ctx, c.namespace, metav1.GetOptions{})
	return err
}
