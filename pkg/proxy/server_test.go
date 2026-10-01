package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestConnectMatch(t *testing.T) {
	t.Parallel()

	srv := startProxy(t, mustParse(t, noneRoutes(t, "api.example.com:6443")), nil)
	proxyAddr := srv.Listener.Addr().String()

	assertConnectStatus(t, proxyAddr, "api.example.com:6443", http.StatusOK)
	assertConnectStatus(t, proxyAddr, "api.example.com", http.StatusForbidden)
	assertConnectStatus(t, proxyAddr, "api.example.com:443", http.StatusForbidden)
	assertConnectStatus(t, proxyAddr, "203.0.113.5:6443", http.StatusForbidden)
}

func TestPlaintextUnknownHost(t *testing.T) {
	t.Parallel()

	srv := startProxy(t, mustParse(t, noneRoutes(t, "api.example.com:443")), nil)
	resp, err := proxyClient(t, srv.URL).Do(newReq(t, "http://203.0.113.9:53/"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "domain not allowed")
}

func TestKubernetesStripInjectAndDenylist(t *testing.T) {
	t.Parallel()

	const investigation = "investigation-token"
	seen := &headerLog{}
	upstreamCACert, _, upstreamCA, upstreamKey := generateCA(t, elliptic.P256())
	upstream := tlsUpstream(t, upstreamCA, upstreamKey, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.WriteHeader(http.StatusOK)
	}))

	host := upstream.Listener.Addr().String()
	cfg := kubeConfig(t, host, upstreamCACert)
	routes := mustProxyRoutes(t, cfg, writeKubeconfig(t, cfg))
	proxyCA, proxyKey, proxyCert, _ := generateCA(t, elliptic.P256())
	srv := startProxy(t, routes, &caFiles{cert: proxyCA, key: proxyKey})

	headers := stolenHeaders()
	assert.Equal(t, http.StatusOK, doProxyTrusting(t, srv.URL, upstream.URL+"/api/v1/namespaces/ns/pods/p/log?watch=true", headers, proxyCert))
	assert.Equal(t, http.StatusOK, doProxyTrusting(t, srv.URL, upstream.URL+"/openapi/v2", headers, proxyCert))

	for _, path := range []string{
		"/api/v1/namespaces/ns/pods/p/exec",
		"/api/v1/namespaces/ns/pods/p/attach",
		"/api/v1/namespaces/ns/pods/p/portforward",
		"/api/v1/namespaces/ns/pods/p/proxy/metrics",
	} {
		assert.Equal(t, http.StatusForbidden, doProxyTrusting(t, srv.URL, upstream.URL+path, headers, proxyCert), path)
	}
	assert.Equal(t, http.StatusForbidden, doProxy(t, srv.URL, "http://"+host+"/api", headers))

	require.Len(t, seen.snapshot(), 2)
	got := seen.snapshot()[0]
	assert.Equal(t, "Bearer "+investigation, got.Get("Authorization"))
	assert.Equal(t, "goog-key", got.Get("X-Goog-Api-Key"))
	assert.Empty(t, got.Get("X-Api-Key"))
	assert.Empty(t, got.Get("Proxy-Authorization"))
	assert.Empty(t, got.Get("Impersonate-User"))
	assert.Empty(t, got.Get("Impersonate-Group"))
	assert.Empty(t, got.Get("Impersonate-Uid"))
	assert.Empty(t, got.Get("Impersonate-Extra-scopes"))
	assert.Empty(t, got.Get("Via"))
	assert.Empty(t, got.Get("X-Forwarded-For"))
	assert.NotContains(t, got.Get("Authorization"), "stolen")
}

func TestNoneStripsWithoutDenylist(t *testing.T) {
	t.Parallel()

	seen := &headerLog{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	host := upstream.Listener.Addr().String()
	srv := startProxy(t, mustParse(t, noneRoutes(t, host)), nil)
	assert.Equal(t, http.StatusNoContent, doProxy(t, srv.URL, upstream.URL+"/exec", stolenHeaders()))
	require.Len(t, seen.snapshot(), 1)
	got := seen.snapshot()[0]
	assert.Empty(t, got.Get("Authorization"))
	assert.Empty(t, got.Get("Impersonate-User"))
	assert.Equal(t, "goog-key", got.Get("X-Goog-Api-Key"))
}

func TestNoneMITMUsesProxyCA(t *testing.T) {
	t.Parallel()

	var hits int
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	upstreamCACert, upstreamCAKeyPEM, upstreamCA, upstreamKey := generateCA(t, elliptic.P256())
	_ = upstreamCAKeyPEM
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{signServerCert(t, upstreamCA, upstreamKey, net.ParseIP("127.0.0.1"))},
		MinVersion:   tls.VersionTLS12,
	}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	host := upstream.Listener.Addr().String()
	proxyCA, proxyKey, proxyCert, _ := generateCA(t, elliptic.P256())
	srv := startProxy(t, mustParse(t, noneRoutes(t, host)), &caFiles{cert: proxyCA, key: proxyKey})

	conn := connectOK(t, srv.Listener.Addr().String(), host)
	tlsConn := tlsClient(t, conn, proxyCert, "127.0.0.1")
	require.NoError(t, tlsConn.HandshakeContext(t.Context()))
	state := tlsConn.ConnectionState()
	require.NotEmpty(t, state.PeerCertificates)
	require.NoError(t, state.PeerCertificates[0].CheckSignatureFrom(proxyCert))

	upstreamPool := x509.NewCertPool()
	require.True(t, upstreamPool.AppendCertsFromPEM(upstreamCACert))
	raw := connectOK(t, srv.Listener.Addr().String(), host)
	upstreamTLS := tls.Client(raw, &tls.Config{
		RootCAs:    upstreamPool,
		ServerName: "127.0.0.1",
		MinVersion: tls.VersionTLS12,
	})
	require.Error(t, upstreamTLS.HandshakeContext(t.Context()))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/exec", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer stolen")
	require.NoError(t, req.Write(tlsConn))
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err == nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
		assert.NotEqual(t, http.StatusOK, resp.StatusCode)
	}
	assert.Zero(t, hits)
}

func TestKubernetesMITMVerifiesRouteCA(t *testing.T) {
	t.Parallel()

	const investigation = "investigation-token"
	seen := &headerLog{}
	upstreamCACert, _, upstreamCA, upstreamKey := generateCA(t, elliptic.P256())
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.WriteHeader(http.StatusAccepted)
	}))
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{signServerCert(t, upstreamCA, upstreamKey, net.ParseIP("127.0.0.1"))},
		MinVersion:   tls.VersionTLS12,
	}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	host := upstream.Listener.Addr().String()
	kcfg := kubeConfig(t, host, upstreamCACert)
	routes := mustProxyRoutes(t, kcfg, writeKubeconfig(t, kcfg))
	proxyCA, proxyKey, proxyCert, _ := generateCA(t, elliptic.P256())
	srv := startProxy(t, routes, &caFiles{cert: proxyCA, key: proxyKey})

	conn := connectOK(t, srv.Listener.Addr().String(), host)
	tlsConn := tlsClient(t, conn, proxyCert, "127.0.0.1")
	require.NoError(t, tlsConn.HandshakeContext(t.Context()))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/api/v1/namespaces/ns/pods/p/log", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer stolen")
	require.NoError(t, req.Write(tlsConn))
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Len(t, seen.snapshot(), 1)
	assert.Equal(t, "Bearer "+investigation, seen.snapshot()[0].Get("Authorization"))

	denied, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/api/v1/namespaces/ns/pods/p/exec", nil)
	require.NoError(t, err)
	require.NoError(t, denied.Write(tlsConn))
	deniedResp, err := http.ReadResponse(bufio.NewReader(tlsConn), denied)
	require.NoError(t, err)
	t.Cleanup(func() { _ = deniedResp.Body.Close() })
	assert.Equal(t, http.StatusForbidden, deniedResp.StatusCode)
	assert.Len(t, seen.snapshot(), 1)
}

func TestNewServerRejectsCA(t *testing.T) {
	t.Parallel()

	cfg := mustParse(t, noneRoutes(t, "api.example.com:443"))
	certPEM, keyPEM, _, _ := generateCA(t, elliptic.P256())
	logger := slog.New(slog.DiscardHandler)

	_, err := NewServer(cfg, []byte("nope"), keyPEM, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse CA")

	_, err = NewServer(cfg, certPEM, []byte("nope"), logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse CA")

	p384Cert, p384Key, _, _ := generateCA(t, elliptic.P384())
	_, err = NewServer(cfg, p384Cert, p384Key, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "P-256")

	otherCert, _, _, _ := generateCA(t, elliptic.P256())
	_, err = NewServer(cfg, otherCert, keyPEM, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}

func TestNewServerRejectsMissingToken(t *testing.T) {
	t.Parallel()

	caPEM, _, _, _ := generateCA(t, elliptic.P256())
	path := writeKubeconfig(t, kubeConfig(t, "other.example:6443", caPEM))
	body, err := json.Marshal(map[string]any{
		"routes": []kubeconfig.ProxyRoute{{
			Domain:         "api.example.com:6443",
			Injector:       kubeconfig.InjectorKubernetes,
			KubeconfigPath: path,
			CACert:         base64.StdEncoding.EncodeToString(caPEM),
		}},
	})
	require.NoError(t, err)
	certPEM, keyPEM, logger := mustCA(t)
	_, err = NewServer(mustParse(t, body), certPEM, keyPEM, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no token")
}

func TestKubernetesRoutesUseDistinctTokens(t *testing.T) {
	t.Parallel()

	caPEM, _, ca, key := generateCA(t, elliptic.P256())
	newUpstream := func() (*httptest.Server, *headerLog) {
		t.Helper()
		seen := &headerLog{}
		srv := tlsUpstream(t, ca, key, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.record(r)
			w.WriteHeader(http.StatusAccepted)
		}))
		return srv, seen
	}
	upA, seenA := newUpstream()
	upB, seenB := newUpstream()
	cfg := &clientcmdapi.Config{
		CurrentContext: "a",
		Clusters: map[string]*clientcmdapi.Cluster{
			"a": {Server: "https://" + upA.Listener.Addr().String(), CertificateAuthorityData: caPEM},
			"b": {Server: "https://" + upB.Listener.Addr().String(), CertificateAuthorityData: caPEM},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"ua": {Token: "token-a"},
			"ub": {Token: "token-b"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"a": {Cluster: "a", AuthInfo: "ua"},
			"b": {Cluster: "b", AuthInfo: "ub"},
		},
	}
	proxyCA, proxyKey, proxyCert, _ := generateCA(t, elliptic.P256())
	srv := startProxy(t, mustProxyRoutes(t, cfg, writeKubeconfig(t, cfg)), &caFiles{cert: proxyCA, key: proxyKey})

	assert.Equal(t, http.StatusAccepted, doProxyTrusting(t, srv.URL, upA.URL+"/api", stolenHeaders(), proxyCert))
	assert.Equal(t, http.StatusAccepted, doProxyTrusting(t, srv.URL, upB.URL+"/api", stolenHeaders(), proxyCert))
	require.Len(t, seenA.snapshot(), 1)
	require.Len(t, seenB.snapshot(), 1)
	assert.Equal(t, "Bearer token-a", seenA.snapshot()[0].Get("Authorization"))
	assert.Equal(t, "Bearer token-b", seenB.snapshot()[0].Get("Authorization"))
	assert.Empty(t, seenA.snapshot()[0].Get("X-Api-Key"))
	assert.Empty(t, seenB.snapshot()[0].Get("Impersonate-User"))
}

func TestHandlerUsesHostHeaderWhenURLHostEmpty(t *testing.T) {
	t.Parallel()

	caPEM, keyPEM, _, _ := generateCA(t, elliptic.P256())
	cfg := kubeConfig(t, "api.example.com:6443", caPEM)
	proxy, err := NewServer(mustProxyRoutes(t, cfg, writeKubeconfig(t, cfg)), caPEM, keyPEM, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(proxy.CloseIdleConnections)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.example.com:6443/exec", nil)
	req.URL.Host = ""
	req.Host = "api.example.com:6443"
	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "path not allowed")
}

func TestNewServerRejectsBadKubeconfig(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, logger := mustCA(t)
	ca := testCACert(t)

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		body, err := json.Marshal(map[string]any{
			"routes": []kubeconfig.ProxyRoute{{
				Domain:         "api.example.com:6443",
				Injector:       kubeconfig.InjectorKubernetes,
				KubeconfigPath: filepath.Join(t.TempDir(), "missing"),
				CACert:         ca,
			}},
		})
		require.NoError(t, err)
		_, err = NewServer(mustParse(t, body), certPEM, keyPEM, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read kubeconfig")
	})

	t.Run("exec auth", func(t *testing.T) {
		t.Parallel()
		cfg := kubeConfig(t, "api.example.com:6443", []byte(ca))
		cfg.AuthInfos["u"].Exec = &clientcmdapi.ExecConfig{Command: "oc"}
		body, err := json.Marshal(map[string]any{
			"routes": []kubeconfig.ProxyRoute{{
				Domain:         "api.example.com:6443",
				Injector:       kubeconfig.InjectorKubernetes,
				KubeconfigPath: writeKubeconfig(t, cfg),
				CACert:         ca,
			}},
		})
		require.NoError(t, err)
		_, err = NewServer(mustParse(t, body), certPEM, keyPEM, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exec auth")
	})
}

func TestHandleInjectionFailure(t *testing.T) {
	t.Parallel()

	table := &routeTable{
		cfg: mustParse(t, noneRoutes(t, "api.example.com:6443")),
		injectors: map[string]requestInjector{
			"api.example.com:6443": &kubernetesInjector{tokens: map[string]string{}},
		},
	}
	table.cfg.byHost["api.example.com:6443"] = kubeconfig.ProxyRoute{
		Domain:   "api.example.com:6443",
		Injector: kubeconfig.InjectorKubernetes,
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.com:6443/api", nil)
	_, resp := table.handle(req, slog.New(slog.DiscardHandler))
	require.NotNil(t, resp)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

type caFiles struct {
	cert []byte
	key  []byte
}

func startProxy(t *testing.T, cfg *Config, ca *caFiles) *httptest.Server {
	t.Helper()
	certPEM, keyPEM, _, _ := generateCA(t, elliptic.P256())
	if ca != nil {
		certPEM, keyPEM = ca.cert, ca.key
	}
	proxy, err := NewServer(cfg, certPEM, keyPEM, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	srv := httptest.NewServer(proxy.Handler())
	t.Cleanup(func() {
		srv.Close()
		proxy.CloseIdleConnections()
	})
	return srv
}

func mustCA(t *testing.T) ([]byte, []byte, *slog.Logger) {
	t.Helper()
	cert, key, _, _ := generateCA(t, elliptic.P256())
	return cert, key, slog.New(slog.DiscardHandler)
}

func kubeConfig(t *testing.T, host string, ca []byte) *clientcmdapi.Config {
	t.Helper()
	return &clientcmdapi.Config{
		CurrentContext: "c",
		Clusters: map[string]*clientcmdapi.Cluster{
			"c": {Server: "https://" + host, CertificateAuthorityData: ca},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"u": {Token: "investigation-token"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"c": {Cluster: "c", AuthInfo: "u"},
		},
	}
}

func writeKubeconfig(t *testing.T, cfg *clientcmdapi.Config) string {
	t.Helper()
	data, err := clientcmd.Write(*cfg)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func mustProxyRoutes(t *testing.T, cfg *clientcmdapi.Config, path string) *Config {
	t.Helper()
	data, err := clientcmd.Write(*cfg)
	require.NoError(t, err)
	routes, err := kubeconfig.BuildProxyRoutes(data, path)
	require.NoError(t, err)
	body, err := json.Marshal(struct {
		Routes []kubeconfig.ProxyRoute `json:"routes"`
	}{Routes: routes})
	require.NoError(t, err)
	return mustParse(t, body)
}

func stolenHeaders() http.Header {
	return http.Header{
		"Authorization":            []string{"Bearer stolen"},
		"X-Api-Key":                []string{"client-key"},
		"Proxy-Authorization":      []string{"Basic abc"},
		"Impersonate-User":         []string{"admin"},
		"Impersonate-Group":        []string{"system:masters"},
		"Impersonate-Uid":          []string{"123"},
		"Impersonate-Extra-scopes": []string{"cluster-admin"},
		"X-Goog-Api-Key":           []string{"goog-key"},
		"Via":                      []string{"evil"},
		"X-Forwarded-For":          []string{"1.2.3.4"},
	}
}

func proxyClient(t *testing.T, proxyURL string) *http.Client {
	t.Helper()
	parsed, err := url.Parse(proxyURL)
	require.NoError(t, err)
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(parsed),
		},
	}
}

func newReq(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	return req
}

func tlsUpstream(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{signServerCert(t, ca, caKey, net.ParseIP("127.0.0.1"))},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func doProxy(t *testing.T, proxyURL, rawURL string, headers http.Header) int {
	t.Helper()
	return doProxyTrusting(t, proxyURL, rawURL, headers, nil)
}

func doProxyTrusting(t *testing.T, proxyURL, rawURL string, headers http.Header, ca *x509.Certificate) int {
	t.Helper()
	req := newReq(t, rawURL)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	client := proxyClient(t, proxyURL)
	if ca != nil {
		pool := x509.NewCertPool()
		pool.AddCert(ca)
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func assertConnectStatus(t *testing.T, proxyAddr, target string, status int) {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", proxyAddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, status, resp.StatusCode, target)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func connectOK(t *testing.T, proxyAddr, target string) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", proxyAddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	return bufferedConn{Conn: conn, reader: reader}
}

func tlsClient(t *testing.T, conn net.Conn, ca *x509.Certificate, serverName string) *tls.Conn {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return tls.Client(conn, &tls.Config{
		RootCAs:    pool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	})
}

type headerLog struct {
	mu      sync.Mutex
	headers []http.Header
}

func (h *headerLog) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.headers = append(h.headers, r.Header.Clone())
}

func (h *headerLog) snapshot() []http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]http.Header, len(h.headers))
	copy(out, h.headers)
	return out
}
