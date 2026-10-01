package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
)

// hostTransport dials each route with that route's CA pool. goproxy's
// transport field is *http.Transport, so per-host verification lives in
// DialTLSContext rather than a separate RoundTripper.
type hostTransport struct {
	base  *http.Transport
	pools map[string]*x509.CertPool
}

func newHostTransport(routes map[string]kubeconfig.ProxyRoute) (*hostTransport, error) {
	var system *x509.CertPool
	pools := make(map[string]*x509.CertPool, len(routes))
	for host, route := range routes {
		pool, err := certPool(route, &system)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", host, err)
		}
		pools[host] = pool
	}
	out := &hostTransport{pools: pools}
	out.base = &http.Transport{
		// Direct dial. A nil Proxy would also skip HTTP_PROXY; set it so that
		// stays obvious.
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
		DialContext: (&net.Dialer{
			Timeout: 15 * time.Second,
		}).DialContext,
		DialTLSContext: out.dialTLS,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}
	return out, nil
}

func certPool(route kubeconfig.ProxyRoute, system **x509.CertPool) (*x509.CertPool, error) {
	switch route.Injector {
	case kubeconfig.InjectorKubernetes:
		raw, err := base64.StdEncoding.DecodeString(route.CACert)
		if err != nil {
			return nil, fmt.Errorf("decode caCert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(raw) {
			return nil, fmt.Errorf("caCert is not a PEM certificate")
		}
		return pool, nil
	case kubeconfig.InjectorNone:
		if *system == nil {
			pool, err := x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("load system cert pool: %w", err)
			}
			if pool == nil {
				return nil, fmt.Errorf("system cert pool is empty")
			}
			*system = pool
		}
		return *system, nil
	default:
		return nil, fmt.Errorf("unknown injector %q", route.Injector)
	}
}

func (h *hostTransport) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, err := kubeconfig.CanonicalHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("upstream host: %w", err)
	}
	pool, ok := h.pools[host]
	if !ok {
		return nil, fmt.Errorf("no upstream CA for %s", host)
	}
	serverName, _, err := net.SplitHostPort(host)
	if err != nil {
		return nil, err
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 15 * time.Second},
		Config: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
			ServerName: serverName,
		},
	}
	return dialer.DialContext(ctx, network, addr)
}

func (h *hostTransport) closeIdle() {
	if h == nil || h.base == nil {
		return
	}
	h.base.CloseIdleConnections()
}
