package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildBasePodSpecOverlay(t *testing.T) {
	t.Run("merges class env and skips reserved names", func(t *testing.T) {
		cfg := newTestConfig()
		cfg.Env = []corev1.EnvVar{
			{Name: "AWS_REGION", Value: "us-east-1"},
			{Name: "KUBECONFIG", Value: "/evil"},
			{Name: "HOME", Value: "/evil"},
			{Name: "SANDBOX_AUTH_TOKEN", Value: "stolen"},
			{
				Name: "AWS_SHARED_CREDENTIALS_FILE",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "aws-cli-creds"},
						Key:                  "path",
					},
				},
			},
		}
		cfg.ImagePullPolicy = corev1.PullIfNotPresent
		cfg.CPURequest = "200m"
		cfg.CPULimit = "1"
		cfg.MemoryRequest = "256Mi"
		cfg.MemoryLimit = "1Gi"

		pod := BuildBasePodSpec("cli-mcp-sandbox-warm", cfg)
		container := pod.Spec.Containers[0]

		assert.Equal(t, ComponentSandbox, pod.Labels[LabelComponent])
		assert.Equal(t, testInstance, pod.Labels[LabelInstance])
		_, hasSession := pod.Labels[LabelSessionID]
		assert.False(t, hasSession)
		require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
		assert.False(t, *pod.Spec.AutomountServiceAccountToken)
		assert.Equal(t, corev1.PullIfNotPresent, container.ImagePullPolicy)
		assert.Equal(t, "200m", container.Resources.Requests.Cpu().String())
		assert.Equal(t, "1", container.Resources.Limits.Cpu().String())
		assert.Equal(t, "256Mi", container.Resources.Requests.Memory().String())
		assert.Equal(t, "1Gi", container.Resources.Limits.Memory().String())

		got := envByName(container.Env)
		assert.Equal(t, "/config/kubeconfig", got["KUBECONFIG"].Value)
		assert.Equal(t, "/workspace", got["HOME"].Value)
		assert.Equal(t, "us-east-1", got["AWS_REGION"].Value)
		require.NotNil(t, got["AWS_SHARED_CREDENTIALS_FILE"].ValueFrom)
		assert.Equal(t, "aws-cli-creds", got["AWS_SHARED_CREDENTIALS_FILE"].ValueFrom.SecretKeyRef.Name)
		_, hasToken := got["SANDBOX_AUTH_TOKEN"]
		assert.False(t, hasToken)
	})

	t.Run("uses DefaultConfig resources when overlay quantities are empty", func(t *testing.T) {
		cfg := newTestConfig()
		cfg.CPURequest = ""
		cfg.CPULimit = ""
		cfg.MemoryRequest = ""
		cfg.MemoryLimit = ""

		pod := BuildBasePodSpec("cli-mcp-sandbox-defaults", cfg)
		container := pod.Spec.Containers[0]
		defaults := DefaultConfig()
		assert.Equal(t, defaults.CPURequest, container.Resources.Requests.Cpu().String())
		assert.Equal(t, defaults.CPULimit, container.Resources.Limits.Cpu().String())
		assert.Equal(t, defaults.MemoryRequest, container.Resources.Requests.Memory().String())
		assert.Equal(t, defaults.MemoryLimit, container.Resources.Limits.Memory().String())
	})
}

func TestSelectorsIncludeInstance(t *testing.T) {
	assert.Equal(t,
		"cli-mcp.redhat.com/instance=oc,cli-mcp.redhat.com/component=sandbox",
		SandboxSelector("oc"),
	)
	assert.Equal(t,
		"cli-mcp.redhat.com/instance=oc,cli-mcp.redhat.com/component=sandbox,cli-mcp.redhat.com/session-id=sess-1",
		AssignedSelector("oc", "sess-1"),
	)
	assert.Equal(t,
		"cli-mcp.redhat.com/instance=oc,cli-mcp.redhat.com/component=sandbox,!cli-mcp.redhat.com/session-id",
		UnassignedSelector("oc"),
	)
}

func TestDefaultConfigHasNoIdentityDefaults(t *testing.T) {
	cfg := DefaultConfig()
	assert.Empty(t, cfg.Namespace)
	assert.Empty(t, cfg.InstanceName)
	assert.Empty(t, cfg.ServiceAccountName)
	assert.Empty(t, cfg.KubeconfigSecret)
	assert.Equal(t, "100m", cfg.CPURequest)
	assert.Equal(t, 8090, cfg.AgentPort)
}

func envByName(env []corev1.EnvVar) map[string]corev1.EnvVar {
	out := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		out[e.Name] = e
	}
	return out
}
