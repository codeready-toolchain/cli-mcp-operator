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
	Namespace          string
	ServiceAccountName string
	KubeconfigSecret   string
	AgentPort          int
}

// DefaultConfig returns a SandboxConfig with production-ready defaults.
func DefaultConfig() SandboxConfig {
	return SandboxConfig{
		Image:              "quay.io/codeready-toolchain/cli-mcp-sandbox:latest",
		CPURequest:         "100m",
		CPULimit:           "500m",
		MemoryRequest:      "128Mi",
		MemoryLimit:        "512Mi",
		IdleTimeout:        30 * time.Minute,
		WarmPoolSize:       0,
		Namespace:          "tarsy",
		ServiceAccountName: "cli-mcp-investigation-sa",
		KubeconfigSecret:   "cli-mcp-investigation-kubeconfig",
		AgentPort:          8090,
	}
}
