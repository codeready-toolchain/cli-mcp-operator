package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/yaml"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	rolledBack := assigned.DeepCopy()
	delete(rolledBack.Labels, session.LabelSessionID)
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: &assigned, ObjectNew: rolledBack}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: &assigned, ObjectNew: activity}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: &assigned, ObjectNew: statusOnly}))
	assert.False(t, p.Generic(event.GenericEvent{Object: &assigned}))

	assignedReady := assigned.DeepCopy()
	assignedReady.Status.Phase = corev1.PodRunning
	assignedReady.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: &assigned, ObjectNew: assignedReady}))

	becameReady := unassigned.DeepCopy()
	becameReady.Status.Phase = corev1.PodRunning
	becameReady.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: unassigned, ObjectNew: becameReady}))

	failed := unassigned.DeepCopy()
	failed.Status.Phase = corev1.PodFailed
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: unassigned, ObjectNew: failed}))

	backoff := unassigned.DeepCopy()
	backoff.Status.ContainerStatuses = []corev1.ContainerStatus{{
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingImagePullBackOff}},
	}}
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: unassigned, ObjectNew: backoff}))
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

func TestManagerRoleIsCRDOnly(t *testing.T) {
	t.Parallel()
	role := loadRoleYAML(t, filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	forbidden := []string{
		"pods", "secrets", "services", "serviceaccounts",
		"deployments", "roles", "rolebindings", "networkpolicies",
	}
	for _, rule := range role.Rules {
		for _, res := range forbidden {
			assert.NotContains(t, rule.Resources, res)
		}
	}
}

func TestNamespacedRoleHasChildResources(t *testing.T) {
	t.Parallel()
	role := loadRoleYAML(t, filepath.Join("..", "..", "config", "rbac", "namespaced_role.yaml"))
	var resources []string
	for _, rule := range role.Rules {
		resources = append(resources, rule.Resources...)
		if slices.Contains(rule.Resources, "pods") {
			assert.Contains(t, rule.Verbs, "create")
		}
	}
	for _, want := range []string{
		"pods", "secrets", "services", "serviceaccounts",
		"deployments", "roles", "rolebindings", "networkpolicies",
	} {
		assert.Contains(t, resources, want)
	}
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

func TestOverlayHash(t *testing.T) {
	t.Parallel()
	base := session.SandboxConfig{
		Image:           "img:a",
		CPURequest:      "100m",
		CPULimit:        "500m",
		MemoryRequest:   "128Mi",
		MemoryLimit:     "512Mi",
		ImagePullPolicy: corev1.PullIfNotPresent,
	}
	same, err := overlayHash(base)
	require.NoError(t, err)
	again, err := overlayHash(base)
	require.NoError(t, err)
	assert.Equal(t, same, again)

	otherImg := base
	otherImg.Image = "img:b"
	hImg, err := overlayHash(otherImg)
	require.NoError(t, err)
	assert.NotEqual(t, same, hImg)

	otherEnv := base
	otherEnv.Env = []corev1.EnvVar{{Name: "FOO", Value: "bar"}}
	hEnv, err := overlayHash(otherEnv)
	require.NoError(t, err)
	assert.NotEqual(t, same, hEnv)

	otherCPU := base
	otherCPU.CPURequest = "200m"
	hCPU, err := overlayHash(otherCPU)
	require.NoError(t, err)
	assert.NotEqual(t, same, hCPU)

	otherPull := base
	otherPull.ImagePullPolicy = corev1.PullAlways
	hPull, err := overlayHash(otherPull)
	require.NoError(t, err)
	assert.NotEqual(t, same, hPull)
}

func TestPoolPodUnhealthy(t *testing.T) {
	t.Parallel()
	assert.False(t, poolPodUnhealthy(corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}))
	assert.True(t, poolPodUnhealthy(corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}))
	assert.True(t, poolPodUnhealthy(corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingCrashLoopBackOff}},
		}},
	}}))
	assigned := sandboxPod("p", "sess", time.Now(), time.Now())
	assigned.Status.Phase = corev1.PodFailed
	assert.False(t, poolPodUnhealthy(assigned))
}

func TestPoolReadyGate(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	snap := poolSnapshot{desired: 2, ready: 1}

	ok, reason, _ := poolReadyGate(&climcpv1alpha1.CliMcpInstance{}, snap, now)
	assert.False(t, ok)
	assert.Equal(t, climcpv1alpha1.ReasonWarmPoolNotReady, reason)

	ready := &climcpv1alpha1.CliMcpInstance{}
	ready.Status.WarmPoolDesired = 2
	meta.SetStatusCondition(&ready.Status.Conditions, metav1.Condition{
		Type:               climcpv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             climcpv1alpha1.ReasonReady,
		LastTransitionTime: metav1.NewTime(now),
	})
	ok, reason, _ = poolReadyGate(ready, snap, now)
	assert.True(t, ok)
	assert.Equal(t, climcpv1alpha1.ReasonReady, reason)

	unhealthy := snap
	unhealthy.unhealthy = true
	unhealthy.unhealthyMsg = "warmup failed"
	ok, reason, msg := poolReadyGate(ready, unhealthy, now)
	assert.False(t, ok)
	assert.Equal(t, climcpv1alpha1.ReasonWarmPoolUnhealthy, reason)
	assert.Equal(t, "warmup failed", msg)

	increase := ready.DeepCopy()
	increase.Status.WarmPoolDesired = 1
	ok, reason, _ = poolReadyGate(increase, poolSnapshot{desired: 2, ready: 1}, now)
	assert.False(t, ok)
	assert.Equal(t, climcpv1alpha1.ReasonWarmPoolNotReady, reason)

	deadline := ready.DeepCopy()
	meta.SetStatusCondition(&deadline.Status.Conditions, metav1.Condition{
		Type:               climcpv1alpha1.ConditionWarmPoolReady,
		Status:             metav1.ConditionFalse,
		Reason:             climcpv1alpha1.ReasonWarmPoolNotReady,
		LastTransitionTime: metav1.NewTime(now.Add(-poolReplenishDeadline - time.Second)),
	})
	ok, reason, _ = poolReadyGate(deadline, snap, now)
	assert.False(t, ok)
	assert.Equal(t, climcpv1alpha1.ReasonWarmPoolNotReady, reason)

	decrease := ready.DeepCopy()
	decrease.Status.WarmPoolDesired = 4
	ok, reason, _ = poolReadyGate(decrease, poolSnapshot{desired: 2, ready: 1}, now)
	assert.True(t, ok)
	assert.Equal(t, climcpv1alpha1.ReasonReady, reason)
}

func TestPoolRequeueAfter(t *testing.T) {
	t.Parallel()
	base := func() *climcpv1alpha1.CliMcpInstance {
		return &climcpv1alpha1.CliMcpInstance{
			Status: climcpv1alpha1.CliMcpInstanceStatus{
				Conditions: []metav1.Condition{
					{Type: climcpv1alpha1.ConditionReady, Status: metav1.ConditionTrue},
					{
						Type:               climcpv1alpha1.ConditionWarmPoolReady,
						Status:             metav1.ConditionFalse,
						LastTransitionTime: metav1.NewTime(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)),
					},
				},
			},
		}
	}
	shortfall := poolSnapshot{desired: 2, ready: 1}

	t.Run("uses now parameter, not wall clock", func(t *testing.T) {
		t.Parallel()
		inst := base()
		now := time.Date(2025, 1, 1, 12, 3, 0, 0, time.UTC)
		got := poolRequeueAfter(inst, shortfall, now)
		assert.Equal(t, 2*time.Minute, got, "should be 5m deadline minus 3m elapsed = 2m")
	})

	t.Run("returns 0 when deadline passed", func(t *testing.T) {
		t.Parallel()
		inst := base()
		now := time.Date(2025, 1, 1, 12, 6, 0, 0, time.UTC)
		got := poolRequeueAfter(inst, shortfall, now)
		assert.Equal(t, time.Duration(0), got)
	})

	t.Run("returns 0 when pool fully ready", func(t *testing.T) {
		t.Parallel()
		inst := base()
		now := time.Date(2025, 1, 1, 12, 1, 0, 0, time.UTC)
		full := poolSnapshot{desired: 2, ready: 2}
		assert.Equal(t, time.Duration(0), poolRequeueAfter(inst, full, now))
	})

	t.Run("returns 0 when pool unhealthy", func(t *testing.T) {
		t.Parallel()
		inst := base()
		now := time.Date(2025, 1, 1, 12, 1, 0, 0, time.UTC)
		unhealthy := poolSnapshot{desired: 2, ready: 1, unhealthy: true}
		assert.Equal(t, time.Duration(0), poolRequeueAfter(inst, unhealthy, now))
	})
}

func TestMergeRequeueAfter(t *testing.T) {
	t.Parallel()
	assert.Equal(t, time.Duration(0), mergeRequeueAfter(0, 0))
	assert.Equal(t, time.Minute, mergeRequeueAfter(time.Minute, 0))
	assert.Equal(t, time.Minute, mergeRequeueAfter(0, time.Minute))
	assert.Equal(t, time.Minute, mergeRequeueAfter(2*time.Minute, time.Minute))
}

func TestDeleteUnassignedIfStillUnassigned(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	t.Run("skips when session-id appeared", func(t *testing.T) {
		t.Parallel()
		claimed := sandboxPod("p", "sess", time.Now(), time.Now())
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claimed.DeepCopy()).Build()
		r := &CliMcpInstanceReconciler{Client: c}
		stale := claimed.DeepCopy()
		delete(stale.Labels, session.LabelSessionID)
		require.NoError(t, r.deleteUnassignedIfStillUnassigned(t.Context(), stale))
		got := &corev1.Pod{}
		require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "p", Namespace: "ns"}, got))
		assert.Equal(t, "sess", got.Labels[session.LabelSessionID])
	})

	t.Run("deletes still-unassigned", func(t *testing.T) {
		t.Parallel()
		pod := sandboxPod("p", "", time.Now(), time.Now())
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
		r := &CliMcpInstanceReconciler{Client: c}
		require.NoError(t, r.deleteUnassignedIfStillUnassigned(t.Context(), &pod))
		got := &corev1.Pod{}
		err := c.Get(t.Context(), types.NamespacedName{Name: "p", Namespace: "ns"}, got)
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("skips gracefully on conflict (concurrent claim race)", func(t *testing.T) {
		t.Parallel()
		pod := sandboxPod("p", "", time.Now(), time.Now())
		inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
		conflictOnDelete := interceptor.NewClient(inner, interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewConflict(
					corev1.Resource("pods"), "p", fmt.Errorf("object was modified"))
			},
		})
		r := &CliMcpInstanceReconciler{Client: conflictOnDelete}
		require.NoError(t, r.deleteUnassignedIfStillUnassigned(t.Context(), &pod))
		got := &corev1.Pod{}
		require.NoError(t, inner.Get(t.Context(), types.NamespacedName{Name: "p", Namespace: "ns"}, got))
		assert.Empty(t, got.Labels[session.LabelSessionID])
	})
}

func TestReconcilePoolErrorKeepsDesired(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, climcpv1alpha1.AddToScheme(scheme))

	inst := &climcpv1alpha1.CliMcpInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "oc", Namespace: "ns"},
		Spec: climcpv1alpha1.CliMcpInstanceSpec{
			Sandbox: climcpv1alpha1.SandboxSpec{WarmPoolSize: 2, Image: "img:test"},
		},
	}
	inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst.DeepCopy()).Build()
	c := interceptor.NewClient(inner, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
			return fmt.Errorf("create failed")
		},
	})
	r := &CliMcpInstanceReconciler{Client: c, Scheme: scheme}
	snap, err := r.reconcilePool(t.Context(), inst, true)
	require.Error(t, err)
	assert.Equal(t, int32(2), snap.desired)
}

func TestReconcilePoolRelistsAfterPartialMutate(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, climcpv1alpha1.AddToScheme(scheme))

	inst := &climcpv1alpha1.CliMcpInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "oc", Namespace: "ns"},
		Spec: climcpv1alpha1.CliMcpInstanceSpec{
			Sandbox: climcpv1alpha1.SandboxSpec{WarmPoolSize: 1, Image: "img:test"},
		},
	}
	hash, err := overlayHash((&CliMcpInstanceReconciler{}).poolSandboxConfig(inst))
	require.NoError(t, err)

	now := time.Now()
	p1 := readyPoolPod("old", now.Add(-2*time.Minute), hash)
	p2 := readyPoolPod("mid", now.Add(-time.Minute), hash)
	p3 := readyPoolPod("new", now, hash)

	inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst.DeepCopy(), &p1, &p2, &p3).Build()
	deletes := 0
	c := interceptor.NewClient(inner, interceptor.Funcs{
		Delete: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes++
			if deletes == 1 {
				return inner.Delete(ctx, obj, opts...)
			}
			return fmt.Errorf("delete failed")
		},
	})
	r := &CliMcpInstanceReconciler{Client: c, Scheme: scheme}
	snap, recErr := r.reconcilePool(t.Context(), inst, true)
	require.Error(t, recErr)
	assert.Equal(t, int32(1), snap.desired)
	assert.Equal(t, int32(2), snap.ready, "re-list must see the pod that survived the first surplus delete")
}

func TestReconcilePoolTerminatingOccupiesSlot(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, climcpv1alpha1.AddToScheme(scheme))

	inst := &climcpv1alpha1.CliMcpInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "oc", Namespace: "ns"},
		Spec: climcpv1alpha1.CliMcpInstanceSpec{
			Sandbox: climcpv1alpha1.SandboxSpec{WarmPoolSize: 1, Image: "img:test"},
		},
	}
	hash, err := overlayHash((&CliMcpInstanceReconciler{}).poolSandboxConfig(inst))
	require.NoError(t, err)

	terminating := sandboxPod("dying", "", time.Now(), time.Now())
	terminating.Annotations[sandboxOverlayAnnotation] = hash
	ts := metav1.Now()
	terminating.DeletionTimestamp = &ts
	terminating.Finalizers = []string{"cli-mcp.redhat.com/test"}

	inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst.DeepCopy(), terminating.DeepCopy()).Build()
	c := interceptor.NewClient(inner, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
			t.Fatal("must not create a replacement while a terminating unassigned pod occupies a slot")
			return fmt.Errorf("unexpected create")
		},
	})
	r := &CliMcpInstanceReconciler{Client: c, Scheme: scheme}
	snap, recErr := r.reconcilePool(t.Context(), inst, true)
	require.NoError(t, recErr)
	assert.Equal(t, int32(1), snap.desired)
	assert.Equal(t, int32(0), snap.ready)
}

func sandboxPod(name, sessionID string, created, activity time.Time) corev1.Pod {
	labels := sandboxLabels("oc")
	if sessionID != "" {
		labels[session.LabelSessionID] = sessionID
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ns",
			Labels:            labels,
			CreationTimestamp: metav1.NewTime(created),
			Annotations: map[string]string{
				session.AnnotationCreatedAt:    created.UTC().Format(time.RFC3339),
				session.AnnotationLastActivity: activity.UTC().Format(time.RFC3339),
			},
		},
	}
}

func readyPoolPod(name string, created time.Time, hash string) corev1.Pod {
	pod := sandboxPod(name, "", created, created)
	pod.Annotations[sandboxOverlayAnnotation] = hash
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	return pod
}
