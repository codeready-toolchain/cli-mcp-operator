package session

import corev1 "k8s.io/api/core/v1"

const (
	LabelSessionID = "cli-mcp.redhat.com/session-id"
	LabelComponent = "cli-mcp.redhat.com/component"
	LabelInstance  = "cli-mcp.redhat.com/instance"

	ComponentSandbox = "sandbox"
	ComponentServer  = "server"
	ComponentProxy   = "proxy"

	// ProxyListenPort is the credential proxy Service port. It is not a CRD field.
	ProxyListenPort int32 = 8080
	// NoProxyValue keeps loopback off the proxy. Cluster DNS names are not listed.
	NoProxyValue = "127.0.0.1,localhost,::1"
	// ProxyCAFile is the sandbox path of the proxy CA certificate.
	ProxyCAFile = "/etc/cli-mcp/proxy-ca/ca.crt"
	// KubeconfigPath is KUBECONFIG inside the sandbox.
	KubeconfigPath = "/config/kubeconfig"

	AnnotationCreatedAt    = "cli-mcp.redhat.com/created-at"
	AnnotationLastActivity = "cli-mcp.redhat.com/last-activity"

	WaitingImagePullBackOff = "ImagePullBackOff"
	WaitingCrashLoopBackOff = "CrashLoopBackOff"
	WaitingErrImagePull     = "ErrImagePull"

	//nolint:gosec // G101: K8s resource name prefix, not a credential.
	authSecretNamePrefix = "cli-mcp-sandbox-auth-"
)

// reservedSandboxEnv names are owned by the sandbox builder. Overlay env
// entries with these names are ignored.
var reservedSandboxEnv = map[string]struct{}{
	"KUBECONFIG":         {},
	"HOME":               {},
	"SANDBOX_AUTH_TOKEN": {},
	"HTTP_PROXY":         {},
	"HTTPS_PROXY":        {},
	"NO_PROXY":           {},
	"http_proxy":         {},
	"https_proxy":        {},
	"no_proxy":           {},
	"SSL_CERT_FILE":      {},
	"REQUESTS_CA_BUNDLE": {},
}

// SandboxConfig holds CRD-agnostic tunables for sandbox pod lifecycle.
// InstanceName, Namespace, ServiceAccountName, and KubeconfigSecret have
// no production defaults — callers (flags, tests, operator) must set them.
type SandboxConfig struct {
	Image              string
	CPURequest         string
	CPULimit           string
	MemoryRequest      string
	MemoryLimit        string
	HMACKey            string
	Namespace          string
	InstanceName       string
	ServiceAccountName string
	// KubeconfigSecret is the admin kubeconfig mounted only when proxy flags are unset.
	KubeconfigSecret string
	// DummyKubeconfigConfigMap is the dummy kubeconfig ConfigMap. Set for a kubernetes target.
	DummyKubeconfigConfigMap string
	// ProxyService is the credential proxy Service name. Empty skips HTTPS_PROXY and the fail-closed gate.
	ProxyService string
	// ProxyCASecret is the MITM CA Secret. Required with ProxyService.
	ProxyCASecret   string
	AgentPort       int
	ImagePullPolicy corev1.PullPolicy
	Env             []corev1.EnvVar
}

// DefaultConfig returns resource and agent-port defaults. Identity fields
// (namespace, instance, SA, kubeconfig secret) are left empty.
func DefaultConfig() SandboxConfig {
	return SandboxConfig{
		CPURequest:    "100m",
		CPULimit:      "500m",
		MemoryRequest: "128Mi",
		MemoryLimit:   "512Mi",
		AgentPort:     8090,
	}
}

// SandboxSelector matches sandbox pods for one instance (component + instance).
func SandboxSelector(instance string) string {
	return LabelInstance + "=" + instance + "," + LabelComponent + "=" + ComponentSandbox
}

// AssignedSelector matches assigned sandbox pods for one instance and session.
func AssignedSelector(instance, sessionID string) string {
	return SandboxSelector(instance) + "," + LabelSessionID + "=" + sessionID
}

// AssignedAnySelector matches assigned sandbox pods/secrets for one instance
// (session-id label exists).
func AssignedAnySelector(instance string) string {
	return SandboxSelector(instance) + "," + LabelSessionID
}

// ServerSelector matches MCP server pods for one instance.
func ServerSelector(instance string) string {
	return LabelInstance + "=" + instance + "," + LabelComponent + "=" + ComponentServer
}

// AuthSecretName is the per-session HMAC token Secret created by the MCP
// process. Idle GC and the finalizer must use the same name.
func AuthSecretName(sessionID string) string {
	return authSecretNamePrefix + sessionID
}

// UnassignedSelector matches sandbox pods for one instance that have no session-id.
func UnassignedSelector(instance string) string {
	return SandboxSelector(instance) + ",!" + LabelSessionID
}

// SandboxNetworkPolicyName is the sandbox NetworkPolicy, derived from the instance name.
func SandboxNetworkPolicyName(instance string) string {
	return "cli-mcp-" + instance + "-sandbox"
}
