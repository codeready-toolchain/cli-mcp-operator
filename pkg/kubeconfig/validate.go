package kubeconfig

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type serverIdentity struct {
	token string
	ca    string
}

// Validate parses kubeconfig bytes and requires token-only auth.
// Each server host:port has one token and one CA. Contexts that share a
// server may differ by namespace. Every cluster must be referenced by a context.
func Validate(data []byte) (*clientcmdapi.Config, error) {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("kubeconfig has no clusters")
	}
	if err = validateAuthInfos(cfg); err != nil {
		return nil, err
	}
	hosts, err := indexClusters(cfg)
	if err != nil {
		return nil, err
	}
	if err := assignContextTokens(cfg, hosts); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadTokens returns the investigation bearer token for each server host:port.
func LoadTokens(data []byte) (map[string]string, error) {
	cfg, err := Validate(data)
	if err != nil {
		return nil, err
	}
	hosts, err := indexClusters(cfg)
	if err != nil {
		return nil, err
	}
	if err = assignContextTokens(cfg, hosts); err != nil {
		return nil, err
	}
	tokens := make(map[string]string, len(hosts))
	for host, id := range hosts {
		tokens[host] = id.token
	}
	return tokens, nil
}

func validateAuthInfos(cfg *clientcmdapi.Config) error {
	for name, auth := range cfg.AuthInfos {
		if err := validateAuth(name, auth); err != nil {
			return err
		}
	}
	return nil
}

func validateAuth(name string, auth *clientcmdapi.AuthInfo) error {
	if len(auth.ClientCertificateData) > 0 || auth.ClientCertificate != "" ||
		len(auth.ClientKeyData) > 0 || auth.ClientKey != "" {
		return fmt.Errorf("user %q uses client certificate auth", name)
	}
	if auth.Exec != nil {
		return fmt.Errorf("user %q uses exec auth", name)
	}
	if auth.AuthProvider != nil {
		return fmt.Errorf("user %q uses auth-provider auth", name)
	}
	if auth.Username != "" || auth.Password != "" {
		return fmt.Errorf("user %q uses basic auth", name)
	}
	if auth.TokenFile != "" {
		return fmt.Errorf("user %q uses tokenFile auth", name)
	}
	if auth.Token == "" {
		return fmt.Errorf("user %q has no token", name)
	}
	return nil
}

func indexClusters(cfg *clientcmdapi.Config) (map[string]*serverIdentity, error) {
	hosts := make(map[string]*serverIdentity, len(cfg.Clusters))
	for name, cluster := range cfg.Clusters {
		if err := validateCluster(name, cluster); err != nil {
			return nil, err
		}
		host, err := HostPortFromServerURL(cluster.Server)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: %w", name, err)
		}
		ca := string(cluster.CertificateAuthorityData)
		existing, seen := hosts[host]
		if !seen {
			hosts[host] = &serverIdentity{ca: ca}
			continue
		}
		if existing.ca != ca {
			return nil, fmt.Errorf("server %s has conflicting certificate authorities", host)
		}
	}
	return hosts, nil
}

func validateCluster(name string, cluster *clientcmdapi.Cluster) error {
	if cluster.Server == "" {
		return fmt.Errorf("cluster %q has no server", name)
	}
	if cluster.CertificateAuthority != "" {
		return fmt.Errorf("cluster %q uses a certificate-authority file path", name)
	}
	if cluster.InsecureSkipTLSVerify {
		return fmt.Errorf("cluster %q sets insecure-skip-tls-verify", name)
	}
	if len(cluster.CertificateAuthorityData) == 0 {
		return fmt.Errorf("cluster %q has no certificate-authority-data", name)
	}
	return nil
}

func assignContextTokens(cfg *clientcmdapi.Config, hosts map[string]*serverIdentity) error {
	covered := make(map[string]struct{}, len(cfg.Clusters))
	for ctxName, ctx := range cfg.Contexts {
		cluster, ok := cfg.Clusters[ctx.Cluster]
		if !ok {
			return fmt.Errorf("context %q references unknown cluster %q", ctxName, ctx.Cluster)
		}
		auth, ok := cfg.AuthInfos[ctx.AuthInfo]
		if !ok {
			return fmt.Errorf("context %q references unknown user %q", ctxName, ctx.AuthInfo)
		}
		host, err := HostPortFromServerURL(cluster.Server)
		if err != nil {
			return fmt.Errorf("context %q: %w", ctxName, err)
		}
		id := hosts[host]
		if id.token == "" {
			id.token = auth.Token
		} else if id.token != auth.Token {
			return fmt.Errorf("context %q: server %s has conflicting tokens", ctxName, host)
		}
		covered[ctx.Cluster] = struct{}{}
	}
	for name := range cfg.Clusters {
		if _, ok := covered[name]; !ok {
			return fmt.Errorf("cluster %q has no context token", name)
		}
	}
	for host, id := range hosts {
		if id.token == "" {
			return fmt.Errorf("server %s has no token", host)
		}
	}
	return nil
}
