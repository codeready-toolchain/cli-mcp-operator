package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/proxy"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/version"
)

var shutdownTimeout = 15 * time.Second

func main() {
	fmt.Fprintf(os.Stderr, "cli-mcp-proxy %s (built %s)\n", version.Commit, version.BuildTime)
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "cli-mcp-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("cli-mcp-proxy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "path to proxy route JSON")
	caCertPath := fs.String("ca-cert", "", "path to MITM CA certificate PEM")
	caKeyPath := fs.String("ca-key", "", "path to MITM CA private key PEM")
	listen := fs.String("listen", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *caCertPath == "" || *caKeyPath == "" {
		return errors.New("--config, --ca-cert, and --ca-key are required")
	}
	return serve(*configPath, *caCertPath, *caKeyPath, *listen)
}

func serve(configPath, caCertPath, caKeyPath, listen string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := proxy.LoadConfig(configPath)
	if err != nil {
		return err
	}
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := os.ReadFile(caKeyPath)
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	proxySrv, err := proxy.NewServer(cfg, caCert, caKey, logger)
	if err != nil {
		return err
	}
	tunnels := newMitmTracker()
	httpSrv := &http.Server{
		Handler:           proxySrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state == http.StateHijacked {
				tunnels.add(conn)
			}
		},
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", listen)
	if err != nil {
		return err
	}
	ln = &trackedListener{Listener: ln, onClose: tunnels.remove}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting proxy", "addr", listen)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig.String())
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	proxySrv.CloseIdleConnections()
	// Shutdown does not wait for connections hijacked by CONNECT MITM.
	shutdownErr := httpSrv.Shutdown(ctx)
	tunnels.drain(ctx)
	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	logger.Info("proxy server stopped")
	return nil
}

// mitmTracker follows connections goproxy hijacks for CONNECT MITM.
// http.Server drops them at Hijack, so Shutdown will not close them.
type mitmTracker struct {
	mu       sync.Mutex
	cond     *sync.Cond
	conns    map[net.Conn]struct{}
	draining bool
}

func newMitmTracker() *mitmTracker {
	t := &mitmTracker{conns: map[net.Conn]struct{}{}}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *mitmTracker) add(conn net.Conn) {
	t.mu.Lock()
	if t.draining {
		t.mu.Unlock()
		_ = conn.Close()
		return
	}
	t.conns[conn] = struct{}{}
	t.mu.Unlock()
}

func (t *mitmTracker) remove(conn net.Conn) {
	t.mu.Lock()
	delete(t.conns, conn)
	if len(t.conns) == 0 {
		t.cond.Broadcast()
	}
	t.mu.Unlock()
}

// drain waits for hijacked tunnels to finish. When ctx ends, it closes any
// that are still open so serve can return.
func (t *mitmTracker) drain(ctx context.Context) {
	wakeup := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			t.mu.Lock()
			t.cond.Broadcast()
			t.mu.Unlock()
		case <-wakeup:
		}
	}()

	t.mu.Lock()
	t.draining = true
	for len(t.conns) > 0 && ctx.Err() == nil {
		t.cond.Wait()
	}
	leftover := make([]net.Conn, 0, len(t.conns))
	for conn := range t.conns {
		leftover = append(leftover, conn)
	}
	t.mu.Unlock()
	close(wakeup)

	for _, conn := range leftover {
		_ = conn.Close()
	}
}

type trackedListener struct {
	net.Listener
	onClose func(net.Conn)
}

func (l *trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: conn, onClose: l.onClose}, nil
}

type trackedConn struct {
	net.Conn
	onClose func(net.Conn)
	once    sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.onClose(c) })
	return err
}
