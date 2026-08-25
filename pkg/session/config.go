package session

import corev1 "k8s.io/api/core/v1"

const (
	LabelSessionID = "cli-mcp.redhat.com/session-id"
	LabelComponent = "cli-mcp.redhat.com/component"
	LabelInstance  = "cli-mcp.redhat.com/instance"

	ComponentSandbox = "sandbox"

	AnnotationCreatedAt    = "cli-mcp.redhat.com/created-at"
	AnnotationLastActivity = "cli-mcp.redhat.com/last-activity"
)

// reservedSandboxEnv names are owned by the sandbox builder. Overlay env
// entries with these names are ignored.
var reservedSandboxEnv = map[string]struct{}{
	"KUBECONFIG":         {},
	"HOME":               {},
	"SANDBOX_AUTH_TOKEN": {},
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
	KubeconfigSecret   string
	AgentPort          int
	ImagePullPolicy    corev1.PullPolicy
	Env                []corev1.EnvVar
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

// UnassignedSelector matches sandbox pods for one instance that have no session-id.
func UnassignedSelector(instance string) string {
	return SandboxSelector(instance) + ",!" + LabelSessionID
}
