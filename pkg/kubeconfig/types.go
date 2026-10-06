// Package kubeconfig validates investigation kubeconfigs, builds a dummy
// kubeconfig, and expands proxy routes. It does not import the operator API.
package kubeconfig

// DummyToken replaces every user token in a sanitized kubeconfig.
const DummyToken = "proxy-managed-token"

// Injector is the proxy route credential behavior.
type Injector string

const (
	// InjectorKubernetes replaces Authorization with the investigation token.
	InjectorKubernetes Injector = "kubernetes"
	// InjectorNone strips client credentials and injects nothing.
	InjectorNone Injector = "none"
)

// ProxyRoute is one exact host:port entry in the proxy route JSON.
type ProxyRoute struct {
	Domain         string   `json:"domain"`
	Injector       Injector `json:"injector"`
	KubeconfigPath string   `json:"kubeconfigPath,omitempty"`
	// CACert is the base64 PEM of the cluster CA. Empty means the public trust store.
	CACert string `json:"caCert,omitempty"`
}
