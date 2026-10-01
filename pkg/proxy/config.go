package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
)

// Config is the proxy route list. Load it with ParseConfig or LoadConfig so
// domains are canonical and routes are indexed.
type Config struct {
	Routes []kubeconfig.ProxyRoute `json:"routes"`

	byHost map[string]kubeconfig.ProxyRoute `json:"-"`
}

// ParseConfig parses route JSON and rejects wildcards, suffixes, bare hosts,
// duplicate host:port values, and unknown injectors.
func ParseConfig(data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse config: trailing data")
	}
	if err := cfg.index(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadConfig reads route JSON from path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return ParseConfig(data)
}

// Match reports the route for an exact host:port. A bare host, a different
// port, or an unknown IP does not match.
func (c *Config) Match(host string) (kubeconfig.ProxyRoute, bool) {
	if c == nil || c.byHost == nil {
		return kubeconfig.ProxyRoute{}, false
	}
	key, err := kubeconfig.CanonicalHostPort(host)
	if err != nil {
		return kubeconfig.ProxyRoute{}, false
	}
	route, ok := c.byHost[key]
	return route, ok
}

func (c *Config) index() error {
	if len(c.Routes) == 0 {
		return fmt.Errorf("config has no routes")
	}
	c.byHost = make(map[string]kubeconfig.ProxyRoute, len(c.Routes))
	for i := range c.Routes {
		prepared, err := prepareRoute(c.Routes[i])
		if err != nil {
			return fmt.Errorf("route %d: %w", i, err)
		}
		if _, ok := c.byHost[prepared.Domain]; ok {
			return fmt.Errorf("duplicate domain %s", prepared.Domain)
		}
		c.Routes[i] = prepared
		c.byHost[prepared.Domain] = prepared
	}
	return nil
}

func prepareRoute(route kubeconfig.ProxyRoute) (kubeconfig.ProxyRoute, error) {
	host, err := kubeconfig.CanonicalHostPort(route.Domain)
	if err != nil {
		return kubeconfig.ProxyRoute{}, err
	}
	route.Domain = host
	switch route.Injector {
	case kubeconfig.InjectorKubernetes:
		if route.KubeconfigPath == "" {
			return kubeconfig.ProxyRoute{}, fmt.Errorf("kubernetes route %s requires kubeconfigPath", host)
		}
		if err := validateCACert(route.CACert); err != nil {
			return kubeconfig.ProxyRoute{}, fmt.Errorf("kubernetes route %s: %w", host, err)
		}
	case kubeconfig.InjectorNone:
	default:
		return kubeconfig.ProxyRoute{}, fmt.Errorf("unknown injector %q", route.Injector)
	}
	return route, nil
}

func validateCACert(encoded string) error {
	if encoded == "" {
		return fmt.Errorf("caCert is required")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode caCert: %w", err)
	}
	if !pemCertOK(raw) {
		return fmt.Errorf("caCert is not a PEM certificate")
	}
	return nil
}
