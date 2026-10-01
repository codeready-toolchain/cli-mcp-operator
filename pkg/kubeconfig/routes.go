package kubeconfig

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"slices"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// BuildProxyRoutes emits one kubernetes route per distinct cluster server.
// caCert is the base64 encoding of the original cluster CA.
func BuildProxyRoutes(data []byte, kubeconfigPath string) ([]ProxyRoute, error) {
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("kubeconfig path is empty")
	}
	cfg, err := Validate(data)
	if err != nil {
		return nil, err
	}
	routes, err := routesFromClusters(cfg, kubeconfigPath)
	if err != nil {
		return nil, err
	}
	sortRoutes(routes)
	return routes, nil
}

func routesFromClusters(cfg *clientcmdapi.Config, kubeconfigPath string) ([]ProxyRoute, error) {
	names := make([]string, 0, len(cfg.Clusters))
	for name := range cfg.Clusters {
		names = append(names, name)
	}
	slices.Sort(names)

	seen := make(map[string]struct{}, len(names))
	routes := make([]ProxyRoute, 0, len(names))
	for _, name := range names {
		cluster := cfg.Clusters[name]
		host, err := HostPortFromServerURL(cluster.Server)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: %w", name, err)
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		routes = append(routes, ProxyRoute{
			Domain:         host,
			Injector:       InjectorKubernetes,
			KubeconfigPath: kubeconfigPath,
			CACert:         base64.StdEncoding.EncodeToString(cluster.CertificateAuthorityData),
		})
	}
	return routes, nil
}

// BuildAllowlistRoutes emits one none route per domain. A bare host becomes
// host:443. Duplicate host:port values are rejected.
func BuildAllowlistRoutes(domains []string) ([]ProxyRoute, error) {
	if len(domains) == 0 {
		return nil, fmt.Errorf("allowlist domains are empty")
	}
	seen := make(map[string]struct{}, len(domains))
	routes := make([]ProxyRoute, 0, len(domains))
	for _, domain := range domains {
		host, err := ParseAllowlistDomain(domain)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[host]; ok {
			return nil, fmt.Errorf("duplicate domain %s", host)
		}
		seen[host] = struct{}{}
		routes = append(routes, ProxyRoute{
			Domain:   host,
			Injector: InjectorNone,
		})
	}
	sortRoutes(routes)
	return routes, nil
}

func sortRoutes(routes []ProxyRoute) {
	slices.SortFunc(routes, func(a, b ProxyRoute) int {
		return cmp.Compare(a.Domain, b.Domain)
	})
}
