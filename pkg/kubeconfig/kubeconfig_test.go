package kubeconfig

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestCanonicalHostPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "exact", in: "api.example.com:6443", want: "api.example.com:6443"},
		{name: "lowercase and port", in: "API.Example.COM:0443", want: "api.example.com:443"},
		{name: "ipv6", in: "[2001:DB8::1]:6443", want: "[2001:db8::1]:6443"},
		{name: "bare host", in: "api.example.com", wantErr: "host:port"},
		{name: "other form rejected later by caller", in: "api.example.com:443", want: "api.example.com:443"},
		{name: "wildcard", in: "*.example.com:443", wantErr: "wildcards"},
		{name: "leading dot", in: ".example.com:443", wantErr: "leading-dot"},
		{name: "bad port", in: "api.example.com:0", wantErr: "invalid port"},
		{name: "empty host", in: ":443", wantErr: "empty host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalHostPort(tt.in)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidateAndRoutes(t *testing.T) {
	t.Parallel()

	valid := baseConfig()
	data := mustWrite(t, valid)
	cfg, err := Validate(data)
	require.NoError(t, err)
	assert.Equal(t, "token-1", cfg.AuthInfos["u1"].Token)

	tokens, err := LoadTokens(data)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"api.example.com:6443": "token-1"}, tokens)

	routes, err := BuildProxyRoutes(data, "/etc/kube/config")
	require.NoError(t, err)
	require.Len(t, routes, 1)
	assert.Equal(t, "api.example.com:6443", routes[0].Domain)
	assert.Equal(t, InjectorKubernetes, routes[0].Injector)
	assert.Equal(t, "/etc/kube/config", routes[0].KubeconfigPath)
	decoded, err := base64.StdEncoding.DecodeString(routes[0].CACert)
	require.NoError(t, err)
	assert.Equal(t, []byte("ca-1"), decoded)
}

func TestValidateSharedServerDifferentNamespace(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Contexts["c2"] = &clientcmdapi.Context{Cluster: "c1", AuthInfo: "u1", Namespace: "other"}
	_, err := Validate(mustWrite(t, cfg))
	require.NoError(t, err)
}

func TestValidateTwoServers(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Clusters["c2"] = &clientcmdapi.Cluster{
		Server:                   "https://api.other.example:443",
		CertificateAuthorityData: []byte("ca-2"),
	}
	cfg.AuthInfos["u2"] = &clientcmdapi.AuthInfo{Token: "token-2"}
	cfg.Contexts["c2"] = &clientcmdapi.Context{Cluster: "c2", AuthInfo: "u2", Namespace: "ns2"}

	data := mustWrite(t, cfg)
	tokens, err := LoadTokens(data)
	require.NoError(t, err)
	assert.Equal(t, "token-1", tokens["api.example.com:6443"])
	assert.Equal(t, "token-2", tokens["api.other.example:443"])

	routes, err := BuildProxyRoutes(data, "/kube/config")
	require.NoError(t, err)
	require.Len(t, routes, 2)
	assert.Equal(t, "api.example.com:6443", routes[0].Domain)
	assert.Equal(t, "api.other.example:443", routes[1].Domain)
}

func TestBuildProxyRoutesNotOnlyCurrentContext(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Clusters["c2"] = &clientcmdapi.Cluster{
		Server:                   "https://second.example.com",
		CertificateAuthorityData: []byte("ca-2"),
	}
	cfg.AuthInfos["u2"] = &clientcmdapi.AuthInfo{Token: "token-2"}
	cfg.Contexts["c2"] = &clientcmdapi.Context{Cluster: "c2", AuthInfo: "u2"}
	cfg.CurrentContext = "c1"

	routes, err := BuildProxyRoutes(mustWrite(t, cfg), "/kube/config")
	require.NoError(t, err)
	require.Len(t, routes, 2)
	assert.Equal(t, "api.example.com:6443", routes[0].Domain)
	assert.Equal(t, "second.example.com:443", routes[1].Domain)
}

func TestBuildProxyRoutesDedupesSameServer(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Clusters["c2"] = &clientcmdapi.Cluster{
		Server:                   "https://API.Example.com:6443",
		CertificateAuthorityData: []byte("ca-1"),
	}
	cfg.Contexts["c2"] = &clientcmdapi.Context{Cluster: "c2", AuthInfo: "u1", Namespace: "other"}

	routes, err := BuildProxyRoutes(mustWrite(t, cfg), "/kube/config")
	require.NoError(t, err)
	require.Len(t, routes, 1)
	assert.Equal(t, "api.example.com:6443", routes[0].Domain)
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*clientcmdapi.Config)
		raw     []byte
		wantErr string
	}{
		{
			name:    "unparsed",
			raw:     []byte(":\n"),
			wantErr: "parse kubeconfig",
		},
		{
			name:    "no clusters",
			mutate:  func(cfg *clientcmdapi.Config) { cfg.Clusters = nil },
			wantErr: "no clusters",
		},
		{
			name: "client cert",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.AuthInfos["u1"].ClientCertificateData = []byte("cert")
			},
			wantErr: "client certificate",
		},
		{
			name: "client cert path",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.AuthInfos["u1"].ClientCertificate = "/tmp/cert"
			},
			wantErr: "client certificate",
		},
		{
			name: "exec",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.AuthInfos["u1"].Exec = &clientcmdapi.ExecConfig{Command: "oc"}
			},
			wantErr: "exec auth",
		},
		{
			name: "auth provider",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.AuthInfos["u1"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "oidc"}
			},
			wantErr: "auth-provider",
		},
		{
			name: "basic auth",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.AuthInfos["u1"].Username = "user"
				cfg.AuthInfos["u1"].Password = "pass"
			},
			wantErr: "basic auth",
		},
		{
			name: "token file",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.AuthInfos["u1"].TokenFile = "/var/run/token"
			},
			wantErr: "tokenFile",
		},
		{
			name: "empty token",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.AuthInfos["u1"].Token = ""
			},
			wantErr: "no token",
		},
		{
			name: "empty server",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c1"].Server = ""
			},
			wantErr: "no server",
		},
		{
			name: "http server",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c1"].Server = "http://api:8080"
			},
			wantErr: "https",
		},
		{
			name: "ca file",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c1"].CertificateAuthority = "/etc/kubernetes/ca.crt"
			},
			wantErr: "certificate-authority file",
		},
		{
			name: "insecure",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c1"].InsecureSkipTLSVerify = true
			},
			wantErr: "insecure-skip-tls-verify",
		},
		{
			name: "empty ca",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c1"].CertificateAuthorityData = nil
			},
			wantErr: "certificate-authority-data",
		},
		{
			name: "conflicting tokens",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.AuthInfos["u2"] = &clientcmdapi.AuthInfo{Token: "token-2"}
				cfg.Contexts["c2"] = &clientcmdapi.Context{Cluster: "c1", AuthInfo: "u2"}
			},
			wantErr: "conflicting tokens",
		},
		{
			name: "conflicting cas",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c2"] = &clientcmdapi.Cluster{
					Server:                   "https://api.example.com:6443",
					CertificateAuthorityData: []byte("ca-other"),
				}
				cfg.Contexts["c2"] = &clientcmdapi.Context{Cluster: "c2", AuthInfo: "u1"}
			},
			wantErr: "conflicting certificate authorities",
		},
		{
			name: "cluster without context",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["orphan"] = &clientcmdapi.Cluster{
					Server:                   "https://orphan.example:6443",
					CertificateAuthorityData: []byte("ca-orphan"),
				}
			},
			wantErr: "no context token",
		},
		{
			name: "unknown cluster",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Contexts["c1"].Cluster = "missing"
			},
			wantErr: "unknown cluster",
		},
		{
			name: "unknown user",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Contexts["c1"].AuthInfo = "missing"
			},
			wantErr: "unknown user",
		},
		{
			name: "server without host",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c1"].Server = "https://"
			},
			wantErr: "no host",
		},
		{
			name: "server bad port",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c1"].Server = "https://api.example.com:70000"
			},
			wantErr: "invalid port",
		},
		{
			name: "wildcard server",
			mutate: func(cfg *clientcmdapi.Config) {
				cfg.Clusters["c1"].Server = "https://*.example.com:6443"
			},
			wantErr: "wildcards",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := tt.raw
			if data == nil {
				cfg := baseConfig()
				tt.mutate(cfg)
				data = mustWrite(t, cfg)
			}
			_, err := Validate(data)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), "token-1")
			assert.NotContains(t, err.Error(), "token-2")
		})
	}
}

func TestSanitizeSwapsTokenAndCA(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Contexts["c2"] = &clientcmdapi.Context{Cluster: "c1", AuthInfo: "u1", Namespace: "other"}
	cfg.CurrentContext = "c1"
	original := mustWrite(t, cfg)
	proxyCA := []byte("proxy-ca-pem-sentinel")

	out, err := Sanitize(original, proxyCA)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "token-1")
	assert.Contains(t, string(out), DummyToken)

	loaded, err := clientcmd.Load(out)
	require.NoError(t, err)
	assert.Equal(t, DummyToken, loaded.AuthInfos["u1"].Token)
	assert.Empty(t, loaded.AuthInfos["u1"].TokenFile)
	assert.Equal(t, proxyCA, loaded.Clusters["c1"].CertificateAuthorityData)
	assert.False(t, loaded.Clusters["c1"].InsecureSkipTLSVerify)
	assert.Equal(t, "c1", loaded.CurrentContext)
	assert.Equal(t, "ns", loaded.Contexts["c1"].Namespace)
	assert.Equal(t, "other", loaded.Contexts["c2"].Namespace)
	assert.Equal(t, "https://api.example.com:6443", loaded.Clusters["c1"].Server)
}

func TestSanitizeRejectsEmptyProxyCA(t *testing.T) {
	t.Parallel()

	_, err := Sanitize(mustWrite(t, baseConfig()), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proxy CA")
}

func TestSanitizeRejectsInvalidKubeconfig(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.AuthInfos["u1"].Token = ""
	_, err := Sanitize(mustWrite(t, cfg), []byte("proxy-ca"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no token")
}

func TestBuildProxyRoutesRequiresPath(t *testing.T) {
	t.Parallel()

	t.Run("empty path", func(t *testing.T) {
		t.Parallel()
		_, err := BuildProxyRoutes(mustWrite(t, baseConfig()), "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "kubeconfig path")
	})
	t.Run("invalid kubeconfig", func(t *testing.T) {
		t.Parallel()
		_, err := BuildProxyRoutes([]byte(":\n"), "/var/run/kubeconfig")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse kubeconfig")
	})
}

func TestBuildAllowlistRoutes(t *testing.T) {
	t.Parallel()

	routes, err := BuildAllowlistRoutes([]string{
		"API.Example.com",
		"other.example:8443",
		"[2001:DB8::1]:6443",
		"[2001:db8::2]",
	})
	require.NoError(t, err)
	require.Len(t, routes, 4)
	assert.Equal(t, ProxyRoute{Domain: "[2001:db8::1]:6443", Injector: InjectorNone}, routes[0])
	assert.Equal(t, ProxyRoute{Domain: "[2001:db8::2]:443", Injector: InjectorNone}, routes[1])
	assert.Equal(t, ProxyRoute{Domain: "api.example.com:443", Injector: InjectorNone}, routes[2])
	assert.Equal(t, ProxyRoute{Domain: "other.example:8443", Injector: InjectorNone}, routes[3])
}

func TestBuildAllowlistRoutesRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		domains []string
		wantErr string
	}{
		{name: "empty", domains: nil, wantErr: "empty"},
		{name: "blank domain", domains: []string{"  "}, wantErr: "empty"},
		{name: "wildcard", domains: []string{"*.example.com"}, wantErr: "wildcards"},
		{name: "leading dot", domains: []string{".example.com"}, wantErr: "leading-dot"},
		{name: "bad port", domains: []string{"example.com:70000"}, wantErr: "invalid port"},
		{name: "bare ipv6", domains: []string{"2001:db8::1"}, wantErr: "IPv6"},
		{name: "duplicate", domains: []string{"Example.com", "example.com:443"}, wantErr: "duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildAllowlistRoutes(tt.domains)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func baseConfig() *clientcmdapi.Config {
	return &clientcmdapi.Config{
		CurrentContext: "c1",
		Clusters: map[string]*clientcmdapi.Cluster{
			"c1": {
				Server:                   "https://api.example.com:6443",
				CertificateAuthorityData: []byte("ca-1"),
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"u1": {Token: "token-1"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"c1": {Cluster: "c1", AuthInfo: "u1", Namespace: "ns"},
		},
	}
}

func mustWrite(t *testing.T, cfg *clientcmdapi.Config) []byte {
	t.Helper()
	data, err := clientcmd.Write(*cfg)
	require.NoError(t, err)
	return data
}
