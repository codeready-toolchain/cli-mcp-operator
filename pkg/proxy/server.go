// Package proxy is a MITM forward proxy. Allowed routes are intercepted so
// client credentials can be stripped and, for kubernetes routes, replaced.
package proxy

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
	"github.com/elazarl/goproxy"
)

// Server is an HTTP forward proxy. Every allowed route is MITM'd.
type Server struct {
	handler    http.Handler
	transports *hostTransport
}

// NewServer builds a proxy that allows only the hosts in cfg. cfg must come
// from ParseConfig or LoadConfig. The CA signs in-memory leaf certificates.
func NewServer(cfg *Config, caCertPEM, caKeyPEM []byte, logger *slog.Logger) (*Server, error) {
	if cfg == nil || cfg.byHost == nil {
		return nil, fmt.Errorf("config is not loaded")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cert, key, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, err
	}
	table, err := newRouteTable(cfg)
	if err != nil {
		return nil, err
	}
	tlsCert := &tls.Certificate{
		Certificate: [][]byte{cert.Raw},
		PrivateKey:  key,
		Leaf:        cert,
	}
	return &Server{
		handler:    newGoproxy(tlsCert, table, logger),
		transports: table.transport,
	}, nil
}

// Handler serves CONNECT and plaintext proxy HTTP.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// CloseIdleConnections drops idle upstream connections.
func (s *Server) CloseIdleConnections() {
	if s == nil {
		return
	}
	s.transports.closeIdle()
}

type routeTable struct {
	cfg       *Config
	injectors map[string]requestInjector
	transport *hostTransport
}

func newRouteTable(cfg *Config) (*routeTable, error) {
	transport, err := newHostTransport(cfg.byHost)
	if err != nil {
		return nil, err
	}
	injectors, err := newInjectors(cfg.byHost)
	if err != nil {
		transport.closeIdle()
		return nil, err
	}
	return &routeTable{cfg: cfg, injectors: injectors, transport: transport}, nil
}

func newInjectors(routes map[string]kubeconfig.ProxyRoute) (map[string]requestInjector, error) {
	cache := make(map[string]*kubernetesInjector)
	injectors := make(map[string]requestInjector, len(routes))
	for host, route := range routes {
		switch route.Injector {
		case kubeconfig.InjectorNone:
			injectors[host] = noneInjector{}
		case kubeconfig.InjectorKubernetes:
			inj, err := cachedKubernetesInjector(cache, route.KubeconfigPath)
			if err != nil {
				return nil, err
			}
			if !inj.has(host) {
				return nil, fmt.Errorf("route %s has no token", host)
			}
			injectors[host] = inj
		default:
			return nil, fmt.Errorf("unknown injector %q", route.Injector)
		}
	}
	return injectors, nil
}

func cachedKubernetesInjector(cache map[string]*kubernetesInjector, path string) (*kubernetesInjector, error) {
	if inj, ok := cache[path]; ok {
		return inj, nil
	}
	inj, err := newKubernetesInjector(path)
	if err != nil {
		return nil, err
	}
	cache[path] = inj
	return inj, nil
}

func newGoproxy(ca *tls.Certificate, table *routeTable, logger *slog.Logger) *goproxy.ProxyHttpServer {
	proxy := goproxy.NewProxyHttpServer()
	proxy.Tr = table.transport.base
	tlsCfg := goproxy.TLSConfigFromCA(ca)
	mitm := &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: tlsCfg}
	reject := &goproxy.ConnectAction{Action: goproxy.ConnectReject, TLSConfig: tlsCfg}

	proxy.OnRequest().HandleConnectFunc(
		func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
			if _, ok := table.cfg.Match(host); !ok {
				logger.Warn("blocked CONNECT", "host", host)
				resp := textResponse(ctx.Req, http.StatusForbidden, "domain not allowed")
				defer func() { _ = resp.Body.Close() }()
				ctx.Resp = resp
				return reject, host
			}
			return mitm, host
		},
	)
	proxy.OnRequest().DoFunc(
		func(req *http.Request, _ *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			return table.handle(req, logger)
		},
	)
	return proxy
}

func (t *routeTable) handle(req *http.Request, logger *slog.Logger) (*http.Request, *http.Response) {
	route, ok := t.cfg.Match(requestHost(req))
	if !ok {
		logger.Warn("blocked request", "host", requestHost(req))
		return req, textResponse(req, http.StatusForbidden, "domain not allowed")
	}
	stripAuthHeaders(req)
	if route.Injector == kubeconfig.InjectorKubernetes && kubePathDenied(req.URL.Path) {
		logger.Warn("blocked kube path", "host", route.Domain, "path", req.URL.Path)
		return req, textResponse(req, http.StatusForbidden, "path not allowed")
	}
	if route.Injector == kubeconfig.InjectorKubernetes && req.URL.Scheme != "https" {
		logger.Warn("blocked plaintext kube request", "host", route.Domain)
		return req, textResponse(req, http.StatusForbidden, "https required")
	}
	injector := t.injectors[route.Domain]
	if injector == nil {
		logger.Error("credential injection failed", "domain", route.Domain)
		return req, textResponse(req, http.StatusBadGateway, "credential injection failed")
	}
	if err := injector.Inject(req); err != nil {
		logger.Error("credential injection failed", "domain", route.Domain)
		return req, textResponse(req, http.StatusBadGateway, "credential injection failed")
	}
	req.Header.Del("Via")
	req.Header.Del("X-Forwarded-For")
	return req, nil
}

func textResponse(req *http.Request, status int, msg string) *http.Response {
	if req == nil {
		req = &http.Request{Method: http.MethodGet, Header: make(http.Header)}
	}
	return goproxy.NewResponse(req, goproxy.ContentTypeText, status, msg+"\n")
}
