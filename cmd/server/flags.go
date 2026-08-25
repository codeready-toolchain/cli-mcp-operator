package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func buildSandboxConfig(cfg runConfig, hmacKey string) (session.SandboxConfig, error) {
	if cfg.sandboxImage == "" {
		return session.SandboxConfig{}, fmt.Errorf("--sandbox-image is required")
	}
	if cfg.namespace == "" {
		return session.SandboxConfig{}, fmt.Errorf("--namespace is required")
	}
	if cfg.instanceName == "" {
		return session.SandboxConfig{}, fmt.Errorf("--instance-name is required")
	}
	if cfg.kubeconfigSecret == "" {
		return session.SandboxConfig{}, fmt.Errorf("--kubeconfig-secret is required")
	}
	if cfg.sandboxServiceAccount == "" {
		return session.SandboxConfig{}, fmt.Errorf("--sandbox-service-account is required")
	}

	defaults := session.DefaultConfig()
	cpuRequest, err := parseQuantityFlag("--sandbox-cpu-request", cfg.cpuRequest, defaults.CPURequest)
	if err != nil {
		return session.SandboxConfig{}, err
	}
	cpuLimit, err := parseQuantityFlag("--sandbox-cpu-limit", cfg.cpuLimit, defaults.CPULimit)
	if err != nil {
		return session.SandboxConfig{}, err
	}
	memoryRequest, err := parseQuantityFlag("--sandbox-memory-request", cfg.memoryRequest, defaults.MemoryRequest)
	if err != nil {
		return session.SandboxConfig{}, err
	}
	memoryLimit, err := parseQuantityFlag("--sandbox-memory-limit", cfg.memoryLimit, defaults.MemoryLimit)
	if err != nil {
		return session.SandboxConfig{}, err
	}
	pullPolicy, err := parseImagePullPolicy(cfg.imagePullPolicy)
	if err != nil {
		return session.SandboxConfig{}, err
	}
	env, err := parseSandboxEnv(cfg.sandboxEnv)
	if err != nil {
		return session.SandboxConfig{}, err
	}

	return session.SandboxConfig{
		Image:              cfg.sandboxImage,
		HMACKey:            hmacKey,
		Namespace:          cfg.namespace,
		InstanceName:       cfg.instanceName,
		ServiceAccountName: cfg.sandboxServiceAccount,
		KubeconfigSecret:   cfg.kubeconfigSecret,
		CPURequest:         cpuRequest,
		CPULimit:           cpuLimit,
		MemoryRequest:      memoryRequest,
		MemoryLimit:        memoryLimit,
		ImagePullPolicy:    pullPolicy,
		Env:                env,
		AgentPort:          defaults.AgentPort,
	}, nil
}

func parseQuantityFlag(flag, value, fallback string) (string, error) {
	if value == "" {
		value = fallback
	}
	if _, err := resource.ParseQuantity(value); err != nil {
		return "", fmt.Errorf("%s: invalid quantity %q: %w", flag, value, err)
	}
	return value, nil
}

func parseImagePullPolicy(s string) (corev1.PullPolicy, error) {
	switch corev1.PullPolicy(s) {
	case "", corev1.PullAlways, corev1.PullNever, corev1.PullIfNotPresent:
		return corev1.PullPolicy(s), nil
	default:
		return "", fmt.Errorf("--sandbox-image-pull-policy must be Always, Never, or IfNotPresent")
	}
}

func parseSandboxEnv(raw string) ([]corev1.EnvVar, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var env []corev1.EnvVar
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("--sandbox-env must be JSON []corev1.EnvVar: %w", err)
	}
	return env, nil
}
