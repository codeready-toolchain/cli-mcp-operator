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
	ctx := t.Context()
	for i := range objects {
		_, err := client.CoreV1().Pods(testNamespace).Create(ctx, &objects[i], metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return NewWarmPool(client, newTestConfig(), slog.Default())
}

func unassignedPod(name, ip string, createdAt time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels: map[string]string{
				LabelComponent: ComponentSandbox,
				LabelInstance:  testInstance,
			},
			Annotations: map[string]string{
				AnnotationCreatedAt: createdAt.Format(time.RFC3339),
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

func TestClaimPod(t *testing.T) {
	t.Run("successful claim assigns pod and creates secret", func(t *testing.T) {
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

		ctx := t.Context()
		ip, name, err := pool.ClaimPod(ctx, "session-abc")

		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", ip)
		assert.Equal(t, "warm-claim-1", name)

		claimed, err := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-claim-1", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "session-abc", claimed.Labels[LabelSessionID])

		secret, err := pool.clientset.CoreV1().Secrets(testNamespace).Get(ctx, secretNamePrefix+"session-abc", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "session-abc", secret.Labels[LabelSessionID])
		assert.Equal(t, testInstance, secret.Labels[LabelInstance])
		assert.NotEmpty(t, secret.StringData["token"])
	})

	t.Run("claims oldest pod first", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		older := unassignedPod("warm-old", "127.0.0.1", time.Now().Add(-10*time.Minute))
		newer := unassignedPod("warm-new", "127.0.0.2", time.Now().Add(-1*time.Minute))
		pool := newTestPool(t, older, newer)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())

		_, name, err := pool.ClaimPod(t.Context(), "session-order")

		require.NoError(t, err)
		assert.Equal(t, "warm-old", name)
	})

	t.Run("does not claim a pod from another instance", func(t *testing.T) {
		other := unassignedPod("warm-aws", "127.0.0.1", time.Now().Add(-10*time.Minute))
		other.Labels[LabelInstance] = "aws"
		pool := newTestPool(t, other)

		_, _, err := pool.ClaimPod(t.Context(), "session-cross")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no unassigned pods available")
	})

	t.Run("waits for empty PodIP then claims", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		pod := unassignedPod("warm-no-ip", "", time.Now())
		pool := newTestPool(t, pod)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())
		ctx := t.Context()

		done := make(chan error, 1)
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				p, err := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-no-ip", metav1.GetOptions{})
				if err != nil {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				if _, claimed := p.Labels[LabelSessionID]; !claimed {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				p.Status.PodIP = "127.0.0.1"
				_, err = pool.clientset.CoreV1().Pods(testNamespace).UpdateStatus(ctx, p, metav1.UpdateOptions{})
				done <- err
				return
			}
			done <- fmt.Errorf("pod was not claimed before deadline")
		}()

		ip, name, err := pool.ClaimPod(ctx, "session-wait-ip")

		require.NoError(t, <-done)
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", ip)
		assert.Equal(t, "warm-no-ip", name)
	})

	t.Run("returns error when no unassigned pods exist", func(t *testing.T) {
		pool := newTestPool(t)

		_, _, err := pool.ClaimPod(t.Context(), "session-empty")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no unassigned pods available")
	})

	t.Run("rolls back label on auth secret already exists", func(t *testing.T) {
		pod := unassignedPod("warm-rollback-1", "127.0.0.1", time.Now())
		pool := newTestPool(t, pod)
		ctx := t.Context()

		conflictingSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretNamePrefix + "session-conflict",
				Namespace: testNamespace,
			},
			StringData: map[string]string{"token": "stale-token"},
		}
		_, err := pool.clientset.CoreV1().Secrets(testNamespace).Create(ctx, conflictingSecret, metav1.CreateOptions{})
		require.NoError(t, err)

		_, _, err = pool.ClaimPod(ctx, "session-conflict")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "all unassigned claim attempts failed")

		rolledBack, getErr := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-rollback-1", metav1.GetOptions{})
		require.NoError(t, getErr)
		_, hasSession := rolledBack.Labels[LabelSessionID]
		assert.False(t, hasSession, "session-id label should be removed on rollback")
	})

	t.Run("rolls back on assign failure", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(ts.Close)

		pod := unassignedPod("warm-assign-fail", "127.0.0.1", time.Now())
		pool := newTestPool(t, pod)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())
		ctx := t.Context()

		_, _, err := pool.ClaimPod(ctx, "session-assign-fail")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "all unassigned claim attempts failed")

		rolledBack, getErr := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-assign-fail", metav1.GetOptions{})
		require.NoError(t, getErr)
		_, hasSession := rolledBack.Labels[LabelSessionID]
		assert.False(t, hasSession, "session-id label should be removed on rollback")

		_, secretErr := pool.clientset.CoreV1().Secrets(testNamespace).Get(ctx, secretNamePrefix+"session-assign-fail", metav1.GetOptions{})
		assert.Error(t, secretErr, "secret should be deleted on rollback")
	})

	t.Run("sends correct token in assign request", func(t *testing.T) {
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

		_, _, err := pool.ClaimPod(t.Context(), "session-token-check")

		require.NoError(t, err)
		assert.Equal(t, expectedToken, receivedToken)
	})
}

func TestGetOrCreatePodAlwaysClaims(t *testing.T) {
	t.Run("claims an unassigned pod even without a configured pool size", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		warmPod := unassignedPod("warm-for-session", "127.0.0.1", time.Now())
		client := fake.NewSimpleClientset()
		ctx := t.Context()
		_, err := client.CoreV1().Pods(testNamespace).Create(ctx, &warmPod, metav1.CreateOptions{})
		require.NoError(t, err)

		cfg := newTestConfig()
		cfg.AgentPort = agentPortFromURL(t, ts.URL)

		mgr, err := NewSessionManager(client, cfg, slog.Default())
		require.NoError(t, err)
		require.NotNil(t, mgr.Pool())
		mgr.Pool().SetHTTPClient(ts.Client())

		ip, getErr := mgr.GetOrCreatePod(ctx, "session-from-pool")

		require.NoError(t, getErr)
		assert.Equal(t, "127.0.0.1", ip)

		claimed, _ := client.CoreV1().Pods(testNamespace).Get(ctx, "warm-for-session", metav1.GetOptions{})
		assert.Equal(t, "session-from-pool", claimed.Labels[LabelSessionID])
	})

	t.Run("falls back to on-demand when no unassigned pod exists", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		mgr, err := NewSessionManager(client, newTestConfig(), slog.Default())
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()

		_, getErr := mgr.GetOrCreatePod(ctx, "session-fallback")

		require.Error(t, getErr)
		assert.Contains(t, getErr.Error(), "create sandbox pod")
		assert.Contains(t, getErr.Error(), "wait for pod ready")
		require.ErrorIs(t, getErr, context.DeadlineExceeded)

		pods, listErr := client.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: AssignedSelector(testInstance, "session-fallback"),
		})
		require.NoError(t, listErr)
		assert.NotEmpty(t, pods.Items, "pod should remain after request deadline for sibling waiters")
		_, secretErr := client.CoreV1().Secrets(testNamespace).Get(context.Background(), secretNamePrefix+"session-fallback", metav1.GetOptions{})
		require.NoError(t, secretErr, "auth secret should remain after request deadline")
	})

	t.Run("leaves claimed pod when waitForReady hits request deadline", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		warmPod := unassignedPod("warm-not-ready", "127.0.0.1", time.Now())
		warmPod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}

		client := fake.NewSimpleClientset()
		ctx := t.Context()
		_, err := client.CoreV1().Pods(testNamespace).Create(ctx, &warmPod, metav1.CreateOptions{})
		require.NoError(t, err)

		cfg := newTestConfig()
		cfg.AgentPort = agentPortFromURL(t, ts.URL)

		mgr, err := NewSessionManager(client, cfg, slog.Default())
		require.NoError(t, err)
		mgr.Pool().SetHTTPClient(ts.Client())

		waitCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()

		_, getErr := mgr.GetOrCreatePod(waitCtx, "session-claim-not-ready")

		require.Error(t, getErr)
		assert.Contains(t, getErr.Error(), "claimed pod not ready")
		require.ErrorIs(t, getErr, context.DeadlineExceeded)

		claimed, podErr := client.CoreV1().Pods(testNamespace).Get(context.Background(), "warm-not-ready", metav1.GetOptions{})
		require.NoError(t, podErr, "claimed pod should remain after request deadline")
		assert.Equal(t, "session-claim-not-ready", claimed.Labels[LabelSessionID])
		_, secretErr := client.CoreV1().Secrets(testNamespace).Get(context.Background(), secretNamePrefix+"session-claim-not-ready", metav1.GetOptions{})
		require.NoError(t, secretErr, "auth secret should remain after request deadline")
	})
}
