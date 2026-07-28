package session

import "time"

// SandboxConfig holds all tunables for sandbox pod lifecycle management.
type SandboxConfig struct {
	Image              string
	CPURequest         string
	CPULimit           string
	MemoryRequest      string
	MemoryLimit        string
	IdleTimeout        time.Duration
	HMACKey            string
	WarmPoolSize       int
	ReconcileInterval  time.Duration
	Namespace          string
	ServiceAccountName string
	KubeconfigSecret   string
	AgentPort          int
}

// DefaultConfig returns a SandboxConfig with production-ready defaults.
// Callers must set Image and HMACKey before use; these have no safe defaults.
func DefaultConfig() SandboxConfig {
	return SandboxConfig{
		CPURequest:         "100m",
		CPULimit:           "500m",
		MemoryRequest:      "128Mi",
		MemoryLimit:        "512Mi",
		IdleTimeout:        30 * time.Minute,
		WarmPoolSize:       0,
		ReconcileInterval:  30 * time.Second,
		Namespace:          "tarsy",
		ServiceAccountName: "cli-mcp-investigation-sa",
		KubeconfigSecret:   "cli-mcp-investigation-kubeconfig",
		AgentPort:          8090,
	}
}
