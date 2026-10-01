package kubeconfig

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
)

// Sanitize returns a kubeconfig that keeps clusters, contexts, namespaces, and
// current-context, with every token replaced by DummyToken and every cluster
// CA replaced by proxyCAPEM. The input must already pass Validate.
func Sanitize(data, proxyCAPEM []byte) ([]byte, error) {
	if len(proxyCAPEM) == 0 {
		return nil, fmt.Errorf("proxy CA certificate is empty")
	}
	cfg, err := Validate(data)
	if err != nil {
		return nil, err
	}
	for _, auth := range cfg.AuthInfos {
		auth.Token = DummyToken
		auth.TokenFile = ""
	}
	for _, cluster := range cfg.Clusters {
		cluster.CertificateAuthorityData = append([]byte(nil), proxyCAPEM...)
		cluster.InsecureSkipTLSVerify = false
		cluster.CertificateAuthority = ""
	}
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("write kubeconfig: %w", err)
	}
	return out, nil
}
