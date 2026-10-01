package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunRequiresPaths(t *testing.T) {
	t.Parallel()

	err := run([]string{"--listen", ":0"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--config, --ca-cert, and --ca-key are required")
}

func TestRunHelp(t *testing.T) {
	t.Parallel()

	err := run([]string{"--help"})
	require.ErrorIs(t, err, flag.ErrHelp)
}

func TestServe(t *testing.T) {
	t.Run("missing config", func(t *testing.T) {
		err := serve(filepath.Join(t.TempDir(), "missing.json"), "ca.pem", "ca.key", "127.0.0.1:0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read config")
	})

	t.Run("missing ca cert", func(t *testing.T) {
		dir := t.TempDir()
		configPath := writeRoutes(t, dir)
		err := serve(configPath, filepath.Join(dir, "missing.pem"), filepath.Join(dir, "ca.key"), "127.0.0.1:0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read CA cert")
	})

	t.Run("missing ca key", func(t *testing.T) {
		dir := t.TempDir()
		configPath := writeRoutes(t, dir)
		certPath, _ := writeCA(t, dir)
		err := serve(configPath, certPath, filepath.Join(dir, "missing.key"), "127.0.0.1:0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read CA key")
	})

	t.Run("invalid listen", func(t *testing.T) {
		dir := t.TempDir()
		configPath := writeRoutes(t, dir)
		certPath, keyPath := writeCA(t, dir)
		err := serve(configPath, certPath, keyPath, "127.0.0.1:99999")
		require.Error(t, err)
	})

	t.Run("stops on signal", func(t *testing.T) {
		dir := t.TempDir()
		configPath := writeRoutes(t, dir)
		certPath, keyPath := writeCA(t, dir)
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		done := make(chan error, 1)
		go func() {
			done <- serve(configPath, certPath, keyPath, addr)
		}()

		require.Eventually(t, func() bool {
			conn, dialErr := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(t.Context(), "tcp", addr)
			if dialErr != nil {
				return false
			}
			_ = conn.Close()
			return true
		}, 5*time.Second, 10*time.Millisecond)

		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", addr)
		require.NoError(t, err)
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
		_, err = fmt.Fprintf(conn, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")
		require.NoError(t, err)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()
		require.NoError(t, conn.Close())

		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("serve did not return after SIGTERM")
		}
	})

	t.Run("closes mitm tunnels", func(t *testing.T) {
		prev := shutdownTimeout
		shutdownTimeout = 300 * time.Millisecond
		t.Cleanup(func() { shutdownTimeout = prev })

		dir := t.TempDir()
		configPath := writeRoutes(t, dir)
		certPath, keyPath := writeCA(t, dir)
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		done := make(chan error, 1)
		go func() {
			done <- serve(configPath, certPath, keyPath, addr)
		}()
		require.Eventually(t, func() bool {
			conn, dialErr := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(t.Context(), "tcp", addr)
			if dialErr != nil {
				return false
			}
			_ = conn.Close()
			return true
		}, 5*time.Second, 10*time.Millisecond)

		tunnel := openMITM(t, addr, certPath)
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

		started := time.Now()
		require.NoError(t, tunnel.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, err = tunnel.Read(make([]byte, 1))
		require.Error(t, err)
		assert.Less(t, time.Since(started), time.Second)
		require.NoError(t, tunnel.Close())

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("serve did not return after SIGTERM")
		}
	})
}

func TestMitmTrackerAddDuringDrainCloses(t *testing.T) {
	t.Parallel()

	tracker := newMitmTracker()
	earlyServer, earlyClient := net.Pipe()
	t.Cleanup(func() {
		_ = earlyServer.Close()
		_ = earlyClient.Close()
	})
	tracker.add(earlyServer)
	require.NoError(t, earlyClient.SetReadDeadline(time.Now().Add(30*time.Millisecond)))
	_, err := earlyClient.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		tracker.drain(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		return tracker.draining
	}, time.Second, 5*time.Millisecond)

	lateServer, lateClient := net.Pipe()
	t.Cleanup(func() {
		_ = lateServer.Close()
		_ = lateClient.Close()
	})
	tracker.add(lateServer)
	_, err = lateClient.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drain did not return")
	}
	_, err = earlyClient.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

func openMITM(t *testing.T, addr, certPath string) *tls.Conn {
	t.Helper()
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	require.NoError(t, raw.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(raw, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")
	require.NoError(t, err)
	reader := bufio.NewReader(raw)
	resp, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	pemBytes, err := os.ReadFile(certPath)
	require.NoError(t, err)
	block, _ := pem.Decode(pemBytes)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	tunnel := tls.Client(&prefixConn{Conn: raw, reader: reader}, &tls.Config{
		RootCAs:    pool,
		ServerName: "api.example.com",
		MinVersion: tls.VersionTLS12,
	})
	require.NoError(t, tunnel.HandshakeContext(t.Context()))
	return tunnel
}

type prefixConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c prefixConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func writeRoutes(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "routes.json")
	body := []byte(`{"routes":[{"domain":"api.example.com:443","injector":"none"}]}`)
	require.NoError(t, os.WriteFile(path, body, 0o600))
	return path
}

func writeCA(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "CLI MCP Proxy Test CA"},
		NotBefore:             time.Now().Add(-31 * 24 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPath = filepath.Join(dir, "ca.crt")
	keyPath = filepath.Join(dir, "ca.key")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}
