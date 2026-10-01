package proxy

import (
	"fmt"
	"net/http"
	"os"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
)

type requestInjector interface {
	Inject(req *http.Request) error
}

type noneInjector struct{}

func (noneInjector) Inject(*http.Request) error { return nil }

type kubernetesInjector struct {
	tokens map[string]string
}

func newKubernetesInjector(path string) (*kubernetesInjector, error) {
	// Path is the kubeconfig the operator mounted for this route.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	tokens, err := kubeconfig.LoadTokens(data)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return &kubernetesInjector{tokens: tokens}, nil
}

func (k *kubernetesInjector) Inject(req *http.Request) error {
	host, err := kubeconfig.CanonicalHostPort(requestHost(req))
	if err != nil {
		return fmt.Errorf("request host: %w", err)
	}
	token, ok := k.tokens[host]
	if !ok || token == "" {
		return fmt.Errorf("no token for host %s", host)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (k *kubernetesInjector) has(host string) bool {
	_, ok := k.tokens[host]
	return ok
}

func requestHost(req *http.Request) string {
	if req.URL != nil && req.URL.Host != "" {
		return req.URL.Host
	}
	return req.Host
}
