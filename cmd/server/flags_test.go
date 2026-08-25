package main

import (
	"testing"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func validRunConfig() runConfig {
	return runConfig{ //nolint:gosec // G101: K8s Secret resource name, not a credential
		sandboxImage:          "quay.io/example/sandbox:test",
		namespace:             "cli-mcp",
		instanceName:          "oc",
		kubeconfigSecret:      "cli-mcp-oc-kubeconfig",
		sandboxServiceAccount: "cli-mcp-oc-sandbox",
	}
}

func TestBuildSandboxConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*runConfig)
		wantErr string
		check   func(*testing.T, session.SandboxConfig)
	}{
		{
			name: "requires instance-name",
			mutate: func(c *runConfig) {
				c.instanceName = ""
			},
			wantErr: "--instance-name is required",
		},
		{
			name: "requires kubeconfig-secret",
			mutate: func(c *runConfig) {
				c.kubeconfigSecret = ""
			},
			wantErr: "--kubeconfig-secret is required",
		},
		{
			name: "requires sandbox-service-account",
			mutate: func(c *runConfig) {
				c.sandboxServiceAccount = ""
			},
			wantErr: "--sandbox-service-account is required",
		},
		{
			name: "requires namespace",
			mutate: func(c *runConfig) {
				c.namespace = ""
			},
			wantErr: "--namespace is required",
		},
		{
			name: "requires sandbox-image",
			mutate: func(c *runConfig) {
				c.sandboxImage = ""
			},
			wantErr: "--sandbox-image is required",
		},
		{
			name: "uses DefaultConfig resources when quantity flags are empty",
			check: func(t *testing.T, cfg session.SandboxConfig) {
				t.Helper()
				defaults := session.DefaultConfig()
				assert.Equal(t, defaults.CPURequest, cfg.CPURequest)
				assert.Equal(t, defaults.CPULimit, cfg.CPULimit)
				assert.Equal(t, defaults.MemoryRequest, cfg.MemoryRequest)
				assert.Equal(t, defaults.MemoryLimit, cfg.MemoryLimit)
				assert.Empty(t, cfg.ImagePullPolicy)
				assert.Empty(t, cfg.Env)
			},
		},
		{
			name: "parses overlay flags",
			mutate: func(c *runConfig) {
				c.cpuRequest = "200m"
				c.cpuLimit = "1"
				c.memoryRequest = "256Mi"
				c.memoryLimit = "1Gi"
				c.imagePullPolicy = "IfNotPresent"
				c.sandboxEnv = `[{"name":"AWS_REGION","value":"us-east-1"}]`
			},
			check: func(t *testing.T, cfg session.SandboxConfig) {
				t.Helper()
				assert.Equal(t, "200m", cfg.CPURequest)
				assert.Equal(t, "1", cfg.CPULimit)
				assert.Equal(t, "256Mi", cfg.MemoryRequest)
				assert.Equal(t, "1Gi", cfg.MemoryLimit)
				assert.Equal(t, corev1.PullIfNotPresent, cfg.ImagePullPolicy)
				require.Len(t, cfg.Env, 1)
				assert.Equal(t, "AWS_REGION", cfg.Env[0].Name)
				assert.Equal(t, "us-east-1", cfg.Env[0].Value)
			},
		},
		{
			name: "rejects invalid pull policy",
			mutate: func(c *runConfig) {
				c.imagePullPolicy = "Sometimes"
			},
			wantErr: "--sandbox-image-pull-policy",
		},
		{
			name: "rejects invalid quantity",
			mutate: func(c *runConfig) {
				c.cpuRequest = "not-a-quantity"
			},
			wantErr: "--sandbox-cpu-request",
		},
		{
			name: "rejects invalid sandbox-env JSON",
			mutate: func(c *runConfig) {
				c.sandboxEnv = "{not json"
			},
			wantErr: "--sandbox-env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validRunConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}

			got, err := buildSandboxConfig(cfg, "hmac")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "oc", got.InstanceName)
			assert.Equal(t, "cli-mcp", got.Namespace)
			assert.Equal(t, "cli-mcp-oc-sandbox", got.ServiceAccountName)
			assert.Equal(t, "cli-mcp-oc-kubeconfig", got.KubeconfigSecret)
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}
