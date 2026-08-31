package controller

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/yaml"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestChildNames(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "cli-mcp-oc", childName("oc"))
	assert.Equal(t, "cli-mcp-oc-hmac", hmacSecretName("oc"))
	assert.Equal(t, "cli-mcp-oc-sandbox", sandboxSAName("oc"))
	assert.Equal(t, "cli-mcp-oc-kubeconfig", kubeconfigSecretName("oc"))
	assert.Equal(t, "cli-mcp-oc-tls", tlsSecretName("oc"))
	// Longest child: cli-mcp- + 44 + -kubeconfig = 63.
	assert.Len(t, kubeconfigSecretName("abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqr"), 63)
}

func TestInstanceFromAdminSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		want   string
		wantOK bool
	}{
		{name: "cli-mcp-oc-kubeconfig", want: "oc", wantOK: true},
		{name: "cli-mcp-oc-tls", want: "oc", wantOK: true},
		{name: "cli-mcp-oc-hmac", want: "oc", wantOK: true},
		{name: "cli-mcp-sandbox-auth-sess", wantOK: false},
		{name: "unrelated", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := instanceFromAdminSecret(tt.name)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSandboxOverlayDefaults(t *testing.T) {
	t.Parallel()
	got := sandboxOverlay(climcpv1alpha1.SandboxSpec{}, "img:tag")
	defaults := session.DefaultConfig()
	assert.Equal(t, "img:tag", got.Image)
	assert.Equal(t, defaults.CPURequest, got.CPURequest)
	assert.Equal(t, defaults.CPULimit, got.CPULimit)
	assert.Equal(t, defaults.MemoryRequest, got.MemoryRequest)
	assert.Equal(t, defaults.MemoryLimit, got.MemoryLimit)
}

func TestSandboxOverlayUsesSpecResources(t *testing.T) {
	t.Parallel()
	got := sandboxOverlay(climcpv1alpha1.SandboxSpec{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
		ImagePullPolicy: corev1.PullIfNotPresent,
	}, "img:tag")
	assert.Equal(t, "200m", got.CPURequest)
	assert.Equal(t, "1", got.CPULimit)
	assert.Equal(t, "256Mi", got.MemoryRequest)
	assert.Equal(t, "1Gi", got.MemoryLimit)
	assert.Equal(t, corev1.PullIfNotPresent, got.ImagePullPolicy)
}

func TestIdleExpiryAndRequeue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	timeout := 30 * time.Minute

	expired := sandboxPod("expired", "s1", now.Add(-time.Hour), now.Add(-time.Hour))
	fresh := sandboxPod("fresh", "s2", now, now)
	unassigned := sandboxPod("warm", "", now.Add(-time.Hour), now.Add(-time.Hour))
	delete(unassigned.Labels, session.LabelSessionID)

	assert.True(t, idleExpired(expired, now, timeout))
	assert.False(t, idleExpired(fresh, now, timeout))
	assert.False(t, idleExpired(unassigned, now, timeout))

	createdOnly := sandboxPod("created", "s3", now.Add(-time.Hour), now)
	delete(createdOnly.Annotations, session.AnnotationLastActivity)
	assert.True(t, idleExpired(createdOnly, now, timeout))

	delay := soonestIdleRequeue([]corev1.Pod{expired, fresh, unassigned}, now, timeout)
	assert.Equal(t, timeout, delay)
}

func TestSandboxPodPredicate(t *testing.T) {
	t.Parallel()
	p := sandboxPodPredicate{}
	assigned := sandboxPod("p", "sess", time.Now(), time.Now())
	unassigned := assigned.DeepCopy()
	delete(unassigned.Labels, session.LabelSessionID)
	activity := assigned.DeepCopy()
	activity.Annotations[session.AnnotationLastActivity] = time.Now().UTC().Format(time.RFC3339)
	statusOnly := assigned.DeepCopy()
	statusOnly.Status.Phase = corev1.PodRunning

	assert.True(t, p.Create(event.CreateEvent{Object: unassigned}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: &assigned}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: unassigned, ObjectNew: &assigned}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: &assigned, ObjectNew: activity}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: &assigned, ObjectNew: statusOnly}))
	assert.False(t, p.Generic(event.GenericEvent{Object: &assigned}))
}

func TestMapSecretIgnoresSessionAuth(t *testing.T) {
	t.Parallel()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "cli-mcp-sandbox-auth-sess",
		Namespace: "ns",
	}}
	assert.Empty(t, mapSecret(t.Context(), secret))

	kube := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "cli-mcp-oc-kubeconfig",
		Namespace: "ns",
	}}
	reqs := mapSecret(t.Context(), kube)
	require.Len(t, reqs, 1)
	assert.Equal(t, "oc", reqs[0].Name)
	assert.Equal(t, "ns", reqs[0].Namespace)

	hmac := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "cli-mcp-oc-hmac",
		Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{{
			Kind: "CliMcpInstance",
			Name: "oc",
		}},
	}}
	reqs = mapSecret(t.Context(), hmac)
	require.Len(t, reqs, 1)
	assert.Equal(t, "oc", reqs[0].Name)
}

func TestManagerRoleHasNoPodsOrSecrets(t *testing.T) {
	t.Parallel()
	role := loadRoleYAML(t, filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	for _, rule := range role.Rules {
		assert.NotContains(t, rule.Resources, "pods")
		assert.NotContains(t, rule.Resources, "secrets")
	}
}

func TestNamespacedRoleHasPodsAndSecrets(t *testing.T) {
	t.Parallel()
	role := loadRoleYAML(t, filepath.Join("..", "..", "config", "rbac", "namespaced_role.yaml"))
	var resources []string
	for _, rule := range role.Rules {
		resources = append(resources, rule.Resources...)
	}
	assert.Contains(t, resources, "pods")
	assert.Contains(t, resources, "secrets")
}

func TestMCPRoleSecretVerbs(t *testing.T) {
	t.Parallel()
	var secretRule *rbacv1.PolicyRule
	for i := range mcpRoleRules() {
		rule := mcpRoleRules()[i]
		if len(rule.Resources) == 1 && rule.Resources[0] == "secrets" {
			secretRule = &rule
		}
	}
	require.NotNil(t, secretRule)
	assert.ElementsMatch(t, []string{"create", "delete"}, secretRule.Verbs)
	assert.NotContains(t, secretRule.Verbs, "get")
	assert.NotContains(t, secretRule.Verbs, "list")
	assert.NotContains(t, secretRule.Verbs, "watch")
}

func TestIdleTimeoutAndReplicasDefaults(t *testing.T) {
	t.Parallel()
	assert.Equal(t, defaultIdleTimeout, idleTimeoutOrDefault(0))
	assert.Equal(t, int32(1), replicasOrDefault(0))
	assert.Equal(t, int32(2), replicasOrDefault(2))
}

func TestMCPServerArgsOmitPoolAndIdle(t *testing.T) {
	t.Parallel()
	inst := &climcpv1alpha1.CliMcpInstance{}
	inst.Name = "oc"
	inst.Namespace = "ns"
	args, err := mcpServerArgs(inst, sandboxOverlay(climcpv1alpha1.SandboxSpec{}, "sandbox:tag"))
	require.NoError(t, err)
	assert.NotContains(t, args, "--warm-pool-size")
	assert.NotContains(t, args, "--idle-timeout")
	assert.Contains(t, args, "--instance-name")
	assert.Contains(t, args, "--kubeconfig-secret")
	assert.Contains(t, args, "--sandbox-service-account")
}

func loadRoleYAML(t *testing.T, path string) rbacv1.Role {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var role rbacv1.Role
	require.NoError(t, yaml.Unmarshal(raw, &role))
	return role
}

func sandboxPod(name, sessionID string, created, activity time.Time) corev1.Pod {
	labels := sandboxLabels("oc")
	if sessionID != "" {
		labels[session.LabelSessionID] = sessionID
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    labels,
			Annotations: map[string]string{
				session.AnnotationCreatedAt:    created.UTC().Format(time.RFC3339),
				session.AnnotationLastActivity: activity.UTC().Format(time.RFC3339),
			},
		},
	}
}
