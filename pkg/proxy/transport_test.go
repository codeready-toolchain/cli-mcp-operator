package proxy

import (
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"testing"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostTransportCAPools(t *testing.T) {
	t.Parallel()

	certPEM, _, _, _ := generateCA(t, elliptic.P256())
	encoded := base64.StdEncoding.EncodeToString(certPEM)
	const (
		pinnedHost = "api.private.example:6443"
		publicHost = "api.public.example:6443"
		allowHost  = "example.com:443"
	)
	transport, err := newHostTransport(map[string]kubeconfig.ProxyRoute{
		pinnedHost: {Injector: kubeconfig.InjectorKubernetes, CACert: encoded},
		publicHost: {Injector: kubeconfig.InjectorKubernetes},
		allowHost:  {Injector: kubeconfig.InjectorNone},
	})
	require.NoError(t, err)

	wantPinned := x509.NewCertPool()
	require.True(t, wantPinned.AppendCertsFromPEM(certPEM))
	assert.True(t, transport.pools[pinnedHost].Equal(wantPinned))

	system, err := x509.SystemCertPool()
	require.NoError(t, err)
	require.NotNil(t, system)
	assert.True(t, transport.pools[publicHost].Equal(system))
	assert.True(t, transport.pools[allowHost].Equal(system))
	assert.False(t, transport.pools[pinnedHost].Equal(system))
}
