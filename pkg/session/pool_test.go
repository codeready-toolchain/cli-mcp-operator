package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestPool(t *testing.T, objects ...corev1.Pod) *WarmPool {
	t.Helper()
	client := fake.NewSimpleClientset()
	ctx := context.Background()
	for i := range objects {
		_, err := client.CoreV1().Pods(testNamespace).Create(ctx, &objects[i], metav1.CreateOptions{})
		require.NoError(t, err)
	}
	cfg := newTestConfig()
	cfg.WarmPoolSize = 3
	return NewWarmPool(client, cfg, slog.Default())
}

func unassignedPod(name, ip string, createdAt time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels: map[string]string{
				labelComponent: componentValue,
			},
			Annotations: map[string]string{
				annotationCreatedAt: createdAt.Format(time.RFC3339),
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func TestBuildWarmPodSpec(t *testing.T) {
	// given
	pool := newTestPool(t)

	// when
	pod := pool.buildWarmPodSpec()

	// then — uses a generated unique name with the standard prefix
	assert.Greater(t, len(pod.Name), len(podNamePrefix), "name should have a random suffix")
	assert.Contains(t, pod.Name, podNamePrefix)
	assert.Empty(t, pod.GenerateName, "should use Name, not GenerateName")
	assert.Equal(t, testNamespace, pod.Namespace)

	// then — has component label but no session-id label
	assert.Equal(t, componentValue, pod.Labels[labelComponent])
	_, hasSessionID := pod.Labels[labelSessionID]
	assert.False(t, hasSessionID, "warm pod must not have session-id label")

	// then — has both created-at and last-activity annotations
	assert.NotEmpty(t, pod.Annotations[annotationCreatedAt])
	assert.NotEmpty(t, pod.Annotations[annotationLastActivity])

	// then — pod security context (no hardcoded UID/GID; OpenShift assigns from namespace range)
	require.NotNil(t, pod.Spec.SecurityContext)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	assert.Nil(t, pod.Spec.SecurityContext.RunAsUser)
	assert.Nil(t, pod.Spec.SecurityContext.RunAsGroup)

	// then — container security
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	assert.Contains(t, container.SecurityContext.Capabilities.Drop, corev1.Capability("ALL"))

	// then — readiness probe (exec to loopback; works with server-only NetworkPolicy)
	require.NotNil(t, container.ReadinessProbe)
	require.NotNil(t, container.ReadinessProbe.Exec)
	require.Equal(t, []string{
		"/bin/bash",
		"-c",
		`timeout 1 bash -c "</dev/tcp/127.0.0.1/8090" && echo "ok" || exit 1`,
	}, container.ReadinessProbe.Exec.Command)
	assert.Nil(t, container.ReadinessProbe.HTTPGet)

	// then — volumes and mounts match session manager
	assert.Len(t, pod.Spec.Volumes, 2)
	assert.Len(t, container.VolumeMounts, 2)

	// then — no SANDBOX_AUTH_TOKEN env var
	envNames := make(map[string]bool)
	for _, e := range container.Env {
		envNames[e.Name] = true
	}
	assert.True(t, envNames["KUBECONFIG"])
	assert.True(t, envNames["HOME"])
	assert.False(t, envNames["SANDBOX_AUTH_TOKEN"], "warm pod must not have SANDBOX_AUTH_TOKEN")
}

func TestReconcilePool(t *testing.T) {
	t.Run("creates pods to reach target size", func(t *testing.T) {
		// given
		pool := newTestPool(t)
		ctx := context.Background()

		// when
		pool.ReconcilePool(ctx)

		// then
		pods, err := pool.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: unassignedSelector(),
		})
		require.NoError(t, err)
		assert.Len(t, pods.Items, 3)

		for _, p := range pods.Items {
			assert.Equal(t, componentValue, p.Labels[labelComponent])
			_, hasSessionID := p.Labels[labelSessionID]
			assert.False(t, hasSessionID)
		}
	})

	t.Run("no-op when pool is full", func(t *testing.T) {
		// given
		existing := []corev1.Pod{
			unassignedPod("warm-1", "10.0.0.1", time.Now()),
			unassignedPod("warm-2", "10.0.0.2", time.Now()),
			unassignedPod("warm-3", "10.0.0.3", time.Now()),
		}
		pool := newTestPool(t, existing...)
		ctx := context.Background()

		// when
		pool.ReconcilePool(ctx)

		// then — no additional pods created
		pods, err := pool.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: unassignedSelector(),
		})
		require.NoError(t, err)
		assert.Len(t, pods.Items, 3)
	})

	t.Run("cleans up stale unassigned pods", func(t *testing.T) {
		// given — one stale pod (created 2× IdleTimeout ago) and one fresh
		cfg := newTestConfig()
		staleTime := time.Now().Add(-(2*cfg.IdleTimeout + time.Minute))
		freshTime := time.Now()

		stalePod := unassignedPod("warm-stale", "10.0.0.1", staleTime)
		freshPod := unassignedPod("warm-fresh", "10.0.0.2", freshTime)

		pool := newTestPool(t, stalePod, freshPod)
		ctx := context.Background()

		// when
		pool.ReconcilePool(ctx)

		// then — stale pod deleted; fresh preserved + new pods created to fill deficit
		pods, err := pool.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: unassignedSelector(),
		})
		require.NoError(t, err)

		var names []string
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		assert.NotContains(t, names, "warm-stale", "stale pod should be deleted")
		assert.Contains(t, names, "warm-fresh", "fresh pod should be preserved")
		// fresh pod + 2 new pods = 3 total
		assert.Len(t, pods.Items, 3)
	})

	t.Run("skips terminal pods when counting", func(t *testing.T) {
		// given — a Succeeded (terminal) unassigned pod should not count toward pool size
		terminalPod := unassignedPod("warm-done", "10.0.0.1", time.Now())
		terminalPod.Status.Phase = corev1.PodSucceeded

		pool := newTestPool(t, terminalPod)
		ctx := context.Background()

		// when
		pool.ReconcilePool(ctx)

		// then — should create 3 pods (terminal pod doesn't count)
		pods, err := pool.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: unassignedSelector(),
		})
		require.NoError(t, err)

		nonTerminal := 0
		for _, p := range pods.Items {
			if p.Status.Phase != corev1.PodFailed && p.Status.Phase != corev1.PodSucceeded {
				nonTerminal++
			}
		}
		assert.Equal(t, 3, nonTerminal)
	})
}

func TestClaimPod(t *testing.T) {
	t.Run("successful claim assigns pod and creates secret", func(t *testing.T) {
		// given
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/assign", r.URL.Path)
			assert.Equal(t, http.MethodPost, r.Method)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		pod := unassignedPod("warm-claim-1", "127.0.0.1", time.Now().Add(-5*time.Minute))
		pool := newTestPool(t, pod)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())

		ctx := context.Background()

		// when
		ip, name, err := pool.ClaimPod(ctx, "session-abc")

		// then
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", ip)
		assert.Equal(t, "warm-claim-1", name)

		// then — pod now has session-id label
		claimed, err := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-claim-1", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "session-abc", claimed.Labels[labelSessionID])

		// then — auth secret created
		secret, err := pool.clientset.CoreV1().Secrets(testNamespace).Get(ctx, secretNamePrefix+"session-abc", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "session-abc", secret.Labels[labelSessionID])
		assert.NotEmpty(t, secret.StringData["token"])
	})

	t.Run("claims oldest pod first", func(t *testing.T) {
		// given
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		older := unassignedPod("warm-old", "127.0.0.1", time.Now().Add(-10*time.Minute))
		newer := unassignedPod("warm-new", "127.0.0.2", time.Now().Add(-1*time.Minute))
		pool := newTestPool(t, older, newer)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())

		// when
		_, name, err := pool.ClaimPod(context.Background(), "session-order")

		// then — should claim the older pod
		require.NoError(t, err)
		assert.Equal(t, "warm-old", name)
	})

	t.Run("returns error when pool exhausted", func(t *testing.T) {
		// given — empty pool
		pool := newTestPool(t)

		// when
		_, _, err := pool.ClaimPod(context.Background(), "session-empty")

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "warm pool exhausted")
	})

	t.Run("rolls back label on auth secret already exists", func(t *testing.T) {
		// given — pre-create a conflicting secret so the pool's Create returns AlreadyExists
		pod := unassignedPod("warm-rollback-1", "127.0.0.1", time.Now())
		pool := newTestPool(t, pod)
		ctx := context.Background()

		conflictingSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretNamePrefix + "session-conflict",
				Namespace: testNamespace,
			},
			StringData: map[string]string{"token": "stale-token"},
		}
		_, err := pool.clientset.CoreV1().Secrets(testNamespace).Create(ctx, conflictingSecret, metav1.CreateOptions{})
		require.NoError(t, err)

		// when — claim should fail because pre-existing secret is not trusted
		_, _, err = pool.ClaimPod(ctx, "session-conflict")

		// then — should fail and roll back the label
		require.Error(t, err)
		assert.Contains(t, err.Error(), "warm pool exhausted")

		rolledBack, getErr := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-rollback-1", metav1.GetOptions{})
		require.NoError(t, getErr)
		_, hasSession := rolledBack.Labels[labelSessionID]
		assert.False(t, hasSession, "session-id label should be removed on rollback")
	})

	t.Run("rolls back on assign failure", func(t *testing.T) {
		// given — agent returns 500
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(ts.Close)

		pod := unassignedPod("warm-assign-fail", "127.0.0.1", time.Now())
		pool := newTestPool(t, pod)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())
		ctx := context.Background()

		// when
		_, _, err := pool.ClaimPod(ctx, "session-assign-fail")

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "warm pool exhausted")

		// then — label should be rolled back (no session-id)
		rolledBack, getErr := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-assign-fail", metav1.GetOptions{})
		require.NoError(t, getErr)
		_, hasSession := rolledBack.Labels[labelSessionID]
		assert.False(t, hasSession, "session-id label should be removed on rollback")

		// then — secret should be deleted
		_, secretErr := pool.clientset.CoreV1().Secrets(testNamespace).Get(ctx, secretNamePrefix+"session-assign-fail", metav1.GetOptions{})
		assert.Error(t, secretErr, "secret should be deleted on rollback")
	})

	t.Run("sends correct token in assign request", func(t *testing.T) {
		// given
		cfg := newTestConfig()
		expectedToken := computeToken(cfg.HMACKey, "session-token-check")

		var receivedToken string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Token string `json:"token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			receivedToken = req.Token
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		pod := unassignedPod("warm-token", "127.0.0.1", time.Now())
		pool := newTestPool(t, pod)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())

		// when
		_, _, err := pool.ClaimPod(context.Background(), "session-token-check")

		// then
		require.NoError(t, err)
		assert.Equal(t, expectedToken, receivedToken)
	})

	t.Run("triggers replenishment after claim", func(t *testing.T) {
		// given
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		pod := unassignedPod("warm-replenish", "127.0.0.1", time.Now())
		pool := newTestPool(t, pod)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())

		// when
		_, _, err := pool.ClaimPod(context.Background(), "session-replenish")
		require.NoError(t, err)

		// then — replenish channel should have a signal
		select {
		case <-pool.replenishCh:
			// expected
		default:
			t.Fatal("expected replenish signal after claim")
		}
	})
}

func TestStartReconciler(t *testing.T) {
	t.Run("falls back to 30s default on zero ReconcileInterval", func(t *testing.T) {
		// given
		pool := newTestPool(t)
		pool.config.ReconcileInterval = 0

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// when — should not panic
		pool.StartReconciler(ctx)

		// then — wait briefly and verify initial reconciliation ran
		time.Sleep(200 * time.Millisecond)
		pods, err := pool.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: unassignedSelector(),
		})
		require.NoError(t, err)
		assert.Len(t, pods.Items, 3, "initial reconciliation should create 3 pods")
	})

	t.Run("falls back to 30s default on negative ReconcileInterval", func(t *testing.T) {
		// given
		pool := newTestPool(t)
		pool.config.ReconcileInterval = -5 * time.Second

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// when — should not panic
		pool.StartReconciler(ctx)

		// then — wait briefly and verify initial reconciliation ran
		time.Sleep(200 * time.Millisecond)
		pods, err := pool.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: unassignedSelector(),
		})
		require.NoError(t, err)
		assert.Len(t, pods.Items, 3)
	})

	t.Run("responds to replenish signal", func(t *testing.T) {
		// given — pool with 1 pod (target is 3)
		pod := unassignedPod("warm-existing", "10.0.0.1", time.Now())
		pool := newTestPool(t, pod)
		pool.config.ReconcileInterval = 10 * time.Minute // long interval so ticker doesn't fire

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		pool.StartReconciler(ctx)
		// wait for initial reconciliation to fill pool to 3
		time.Sleep(200 * time.Millisecond)

		// given — delete one pod to create a deficit
		pods, err := pool.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: unassignedSelector(),
		})
		require.NoError(t, err)
		require.NotEmpty(t, pods.Items)
		err = pool.clientset.CoreV1().Pods(testNamespace).Delete(ctx, pods.Items[0].Name, metav1.DeleteOptions{})
		require.NoError(t, err)

		// when — trigger replenish to fill the deficit
		pool.TriggerReplenish()
		time.Sleep(200 * time.Millisecond)

		// then — should be back to 3 pods
		pods, err = pool.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: unassignedSelector(),
		})
		require.NoError(t, err)
		assert.Len(t, pods.Items, 3)
	})

	t.Run("exits on context cancellation", func(t *testing.T) {
		// given
		pool := newTestPool(t)
		pool.config.ReconcileInterval = 50 * time.Millisecond

		ctx, cancel := context.WithCancel(context.Background())
		pool.StartReconciler(ctx)
		time.Sleep(100 * time.Millisecond)

		// when
		cancel()
		time.Sleep(100 * time.Millisecond)

		// then — reconciler goroutine has exited (no way to assert directly,
		// but no panic and clean shutdown is the test)
	})
}

func TestTriggerReplenish(t *testing.T) {
	t.Run("non-blocking when channel is full", func(t *testing.T) {
		// given
		pool := newTestPool(t)

		// when — fill the channel
		pool.TriggerReplenish()
		// second call should not block
		pool.TriggerReplenish()

		// then — channel has exactly 1 signal
		select {
		case <-pool.replenishCh:
			// expected
		default:
			t.Fatal("expected at least one signal in replenish channel")
		}

		select {
		case <-pool.replenishCh:
			t.Fatal("expected only one signal (coalesced)")
		default:
			// expected
		}
	})
}

func TestGetOrCreatePodWithPool(t *testing.T) {
	t.Run("claims from pool when available", func(t *testing.T) {
		// given
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		warmPod := unassignedPod("warm-for-session", "127.0.0.1", time.Now())

		client := fake.NewSimpleClientset()
		ctx := context.Background()
		_, err := client.CoreV1().Pods(testNamespace).Create(ctx, &warmPod, metav1.CreateOptions{})
		require.NoError(t, err)

		cfg := newTestConfig()
		cfg.WarmPoolSize = 3
		cfg.AgentPort = agentPortFromURL(t, ts.URL)

		mgr, err := NewSessionManager(client, cfg, slog.Default())
		require.NoError(t, err)
		require.NotNil(t, mgr.Pool(), "pool should be enabled when WarmPoolSize > 0")
		mgr.Pool().SetHTTPClient(ts.Client())

		// when
		ip, getErr := mgr.GetOrCreatePod(ctx, "session-from-pool")

		// then — should have claimed the warm pod
		require.NoError(t, getErr)
		assert.Equal(t, "127.0.0.1", ip)

		claimed, _ := client.CoreV1().Pods(testNamespace).Get(ctx, "warm-for-session", metav1.GetOptions{})
		assert.Equal(t, "session-from-pool", claimed.Labels[labelSessionID])
	})

	t.Run("falls back to on-demand when pool exhausted", func(t *testing.T) {
		// given — pool enabled but empty
		client := fake.NewSimpleClientset()
		cfg := newTestConfig()
		cfg.WarmPoolSize = 3

		mgr, err := NewSessionManager(client, cfg, slog.Default())
		require.NoError(t, err)

		// when — GetOrCreatePod will fail pool claim and fall through to on-demand
		// On-demand create will make a pod in the fake client.
		// The fake client doesn't set PodIP or conditions, so waitForReady will timeout.
		// We just verify it attempts on-demand (the pod is created in K8s).
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		_, getErr := mgr.GetOrCreatePod(ctx, "session-fallback")

		// then — expect timeout error from waitForReady (on-demand path was taken)
		require.Error(t, getErr)

		// then — verify on-demand pod was created (proves fallback happened)
		pods, listErr := client.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", labelSessionID, "session-fallback"),
		})
		require.NoError(t, listErr)
		assert.NotEmpty(t, pods.Items, "on-demand pod should have been created")
	})

	t.Run("pool disabled when WarmPoolSize is 0", func(t *testing.T) {
		// given
		client := fake.NewSimpleClientset()
		cfg := newTestConfig()
		cfg.WarmPoolSize = 0

		mgr, err := NewSessionManager(client, cfg, slog.Default())
		require.NoError(t, err)

		// then
		assert.Nil(t, mgr.Pool(), "pool should be nil when WarmPoolSize is 0")
	})
}

func TestDefaultConfigReconcileInterval(t *testing.T) {
	// given / when
	cfg := DefaultConfig()

	// then
	assert.Equal(t, 30*time.Second, cfg.ReconcileInterval)
}
