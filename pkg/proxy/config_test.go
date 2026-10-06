package proxy

import (
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchExactHostPort(t *testing.T) {
	t.Parallel()

	cfg := mustParse(t, noneRoutes(t, "api.example.com:6443", "[2001:DB8::1]:6443", "203.0.113.10:6443"))

	tests := []struct {
		host string
		ok   bool
	}{
		{host: "api.example.com:6443", ok: true},
		{host: "API.Example.com:6443", ok: true},
		{host: "api.example.com:443", ok: false},
		{host: "api.example.com", ok: false},
		{host: "[2001:db8::1]:6443", ok: true},
		{host: "[2001:db8::1]:443", ok: false},
		{host: "203.0.113.10:6443", ok: true},
		{host: "203.0.113.11:6443", ok: false},
		{host: "198.51.100.20:6443", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			_, ok := cfg.Match(tt.host)
			assert.Equal(t, tt.ok, ok)
		})
	}
}

func TestParseConfigRejects(t *testing.T) {
	t.Parallel()

	ca := testCACert(t)
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "bare host", body: noneJSON("api.example.com"), wantErr: "host:port"},
		{name: "wildcard", body: noneJSON("*.example.com:443"), wantErr: "wildcards"},
		{name: "leading dot", body: noneJSON(".example.com:443"), wantErr: "leading-dot"},
		{name: "unknown injector", body: `{"routes":[{"domain":"api.example.com:443","injector":"bearer"}]}`, wantErr: "unknown injector"},
		{name: "allowed paths", body: `{"routes":[{"domain":"api.example.com:443","injector":"none","allowedPaths":["/"]}]}`, wantErr: "unknown field"},
		{name: "duplicate", body: noneJSON("API.example.com:443", "api.example.com:0443"), wantErr: "duplicate"},
		{name: "no routes", body: `{"routes":[]}`, wantErr: "no routes"},
		{name: "bad ca", body: `{"routes":[{"domain":"api.example.com:6443","injector":"kubernetes","kubeconfigPath":"/kube/config","caCert":"` + base64.StdEncoding.EncodeToString([]byte("nope")) + `"}]}`, wantErr: "PEM"},
		{name: "missing kubeconfig", body: `{"routes":[{"domain":"api.example.com:6443","injector":"kubernetes","caCert":"` + ca + `"}]}`, wantErr: "kubeconfigPath"},
		{name: "bad ca encoding", body: `{"routes":[{"domain":"api.example.com:6443","injector":"kubernetes","kubeconfigPath":"/kube/config","caCert":"****"}]}`, wantErr: "decode caCert"},
		{name: "ca not a cert", body: `{"routes":[{"domain":"api.example.com:6443","injector":"kubernetes","kubeconfigPath":"/kube/config","caCert":"` + base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nope")})) + `"}]}`, wantErr: "PEM"},
		{name: "trailing data", body: noneJSON("api.example.com:443") + `{"extra":true}`, wantErr: "trailing data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseConfig([]byte(tt.body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "routes.json")
	require.NoError(t, os.WriteFile(path, noneRoutes(t, "API.Example.com:443"), 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	_, ok := cfg.Match("api.example.com:443")
	assert.True(t, ok)

	_, err = LoadConfig(filepath.Join(dir, "missing.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read config")
}

func TestParseConfigKubernetesOmitsCA(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"routes":[{"domain":"api.example.com:6443","injector":"kubernetes","kubeconfigPath":"/etc/kube/config"}]}`))
	require.NoError(t, err)
	route, ok := cfg.Match("api.example.com:6443")
	require.True(t, ok)
	assert.Equal(t, kubeconfig.InjectorKubernetes, route.Injector)
	assert.Empty(t, route.CACert)
	assert.Equal(t, "/etc/kube/config", route.KubeconfigPath)
}

func TestParseConfigKubernetesCA(t *testing.T) {
	t.Parallel()

	ca := testCACert(t)
	cfg, err := ParseConfig([]byte(`{"routes":[{"domain":"API.Example.com:06443","injector":"kubernetes","kubeconfigPath":"/etc/kube/config","caCert":"` + ca + `"}]}`))
	require.NoError(t, err)
	route, ok := cfg.Match("api.example.com:6443")
	require.True(t, ok)
	assert.Equal(t, kubeconfig.InjectorKubernetes, route.Injector)
	assert.Equal(t, "/etc/kube/config", route.KubeconfigPath)
}

func TestParseConfigIgnoresNoneCA(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"routes":[{"domain":"api.example.com:443","injector":"none","caCert":"not-base64"}]}`))
	require.NoError(t, err)
	_, ok := cfg.Match("api.example.com:443")
	assert.True(t, ok)
}

func TestKubePathDenied(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		deny bool
	}{
		{path: "/api/v1/namespaces/ns/pods/p/log", deny: false},
		{path: "/api/v1/namespaces/ns/pods", deny: false},
		{path: "/openapi/v2", deny: false},
		{path: "/api/v1/namespaces/ns/pods/p/exec", deny: true},
		{path: "/api/v1/namespaces/ns/pods/p/attach", deny: true},
		{path: "/api/v1/namespaces/ns/pods/p/portforward", deny: true},
		{path: "/api/v1/namespaces/ns/pods/p/proxy/metrics", deny: true},
		{path: "/api/v1/nodes/node/proxy/stats/summary", deny: true},
		{path: "/api/v1/namespaces/ns/services/svc:443/proxy/health", deny: true},
		{path: "/api/v1/proxy/namespaces/ns/pods/p/log", deny: true},
		{path: "/api/v1/watch/namespaces/ns/pods/p/log", deny: false},
		{path: "/api/v1/watch/namespaces/ns/pods/p/exec", deny: true},
		{path: "/api/v1/namespaces/ns/pods/p/execution", deny: false},
		{path: "/api/v1/namespaces/ns/pods/proxy/log", deny: false},
		{path: "/api/v1/namespaces/exec", deny: false},
		{path: "/api/v1/namespaces/exec/pods", deny: false},
		{path: "/api/v1/namespaces/exec/status", deny: false},
		{path: "/apis/apps/v1/namespaces/ns/deployments/proxy", deny: false},
		{path: "/apis/apps/v1/namespaces/ns/deployments/proxy/status", deny: false},
		{path: "/api/v1/pods/foo/exec/../log", deny: false},
		{path: "/api/v1/pods/foo/log/../../exec", deny: false},
		{path: "/api/v1/namespaces/ns/pods/foo/log/../../p/exec", deny: true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.deny, kubePathDenied(tt.path))
		})
	}
}

func mustParse(t *testing.T, body []byte) *Config {
	t.Helper()
	cfg, err := ParseConfig(body)
	require.NoError(t, err)
	return cfg
}

func noneRoutes(t *testing.T, domains ...string) []byte {
	t.Helper()
	routes := make([]kubeconfig.ProxyRoute, 0, len(domains))
	for _, domain := range domains {
		routes = append(routes, kubeconfig.ProxyRoute{Domain: domain, Injector: kubeconfig.InjectorNone})
	}
	body, err := json.Marshal(struct {
		Routes []kubeconfig.ProxyRoute `json:"routes"`
	}{Routes: routes})
	require.NoError(t, err)
	return body
}

func noneJSON(domains ...string) string {
	routes := make([]map[string]string, 0, len(domains))
	for _, domain := range domains {
		routes = append(routes, map[string]string{"domain": domain, "injector": "none"})
	}
	body, err := json.Marshal(map[string]any{"routes": routes})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func testCACert(t *testing.T) string {
	t.Helper()
	certPEM, _, _, _ := generateCA(t, elliptic.P256())
	return base64.StdEncoding.EncodeToString(certPEM)
}
