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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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

		lastAct, err := time.Parse(time.RFC3339, claimed.Annotations[AnnotationLastActivity])
		require.NoError(t, err, "last-activity annotation must be valid RFC3339")
		assert.WithinDuration(t, time.Now().UTC(), lastAct, 5*time.Second,
			"claim must refresh last-activity to ~now")

		secret, err := pool.clientset.CoreV1().Secrets(testNamespace).Get(ctx, AuthSecretName("session-abc"), metav1.GetOptions{})
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
				Name:      AuthSecretName("session-conflict"),
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

		_, secretErr := pool.clientset.CoreV1().Secrets(testNamespace).Get(ctx, AuthSecretName("session-assign-fail"), metav1.GetOptions{})
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

	t.Run("does not claim unclaimable unassigned pods", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(*corev1.Pod)
		}{
			{
				name: "terminating",
				mutate: func(pod *corev1.Pod) {
					ts := metav1.Now()
					pod.DeletionTimestamp = &ts
					pod.Finalizers = []string{"cli-mcp.redhat.com/test"}
				},
			},
			{
				name: "Failed",
				mutate: func(pod *corev1.Pod) {
					pod.Status.Phase = corev1.PodFailed
				},
			},
			{
				name: "Succeeded",
				mutate: func(pod *corev1.Pod) {
					pod.Status.Phase = corev1.PodSucceeded
				},
			},
			{
				name: "ImagePullBackOff",
				mutate: func(pod *corev1.Pod) {
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: WaitingImagePullBackOff}},
					}}
				},
			},
			{
				name: "CrashLoopBackOff",
				mutate: func(pod *corev1.Pod) {
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: WaitingCrashLoopBackOff}},
					}}
				},
			},
			{
				name: "ErrImagePull",
				mutate: func(pod *corev1.Pod) {
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: WaitingErrImagePull}},
					}}
				},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				pod := unassignedPod("warm-skip", "127.0.0.1", time.Now())
				tt.mutate(&pod)
				pool := newTestPool(t, pod)

				_, _, err := pool.ClaimPod(t.Context(), "session-skip")

				require.Error(t, err)
				assert.Equal(t, "all unassigned claim attempts failed", err.Error())
				got, getErr := pool.clientset.CoreV1().Pods(testNamespace).Get(t.Context(), "warm-skip", metav1.GetOptions{})
				require.NoError(t, getErr)
				_, hasSession := got.Labels[LabelSessionID]
				assert.False(t, hasSession)
			})
		}
	})

	t.Run("skips an unclaimable oldest pod and claims the next ready one", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		backoff := unassignedPod("warm-old-backoff", "10.0.0.9", time.Now().Add(-10*time.Minute))
		backoff.Status.ContainerStatuses = []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: WaitingImagePullBackOff}},
		}}
		ready := unassignedPod("warm-ready", "127.0.0.1", time.Now().Add(-time.Minute))
		pool := newTestPool(t, backoff, ready)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())
		ctx := t.Context()

		ip, name, err := pool.ClaimPod(ctx, "session-skip-next")

		require.NoError(t, err)
		assert.Equal(t, "warm-ready", name)
		assert.Equal(t, "127.0.0.1", ip)
		skipped, getErr := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-old-backoff", metav1.GetOptions{})
		require.NoError(t, getErr)
		_, hasSession := skipped.Labels[LabelSessionID]
		assert.False(t, hasSession)
	})

	t.Run("retries the next pod after a resourceVersion conflict", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		first := unassignedPod("warm-conflict", "10.0.0.1", time.Now().Add(-10*time.Minute))
		second := unassignedPod("warm-ok", "127.0.0.1", time.Now().Add(-time.Minute))
		pool := newTestPool(t, first, second)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())
		cs, ok := pool.clientset.(*fake.Clientset)
		require.True(t, ok)
		cs.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			pa, ok := action.(k8stesting.PatchAction)
			if !ok || pa.GetName() != "warm-conflict" {
				return false, nil, nil
			}
			return true, nil, k8serrors.NewConflict(schema.GroupResource{Resource: "pods"}, "warm-conflict", fmt.Errorf("conflict"))
		})

		ip, name, err := pool.ClaimPod(t.Context(), "session-conflict-retry")

		require.NoError(t, err)
		assert.Equal(t, "warm-ok", name)
		assert.Equal(t, "127.0.0.1", ip)
		lost, getErr := pool.clientset.CoreV1().Pods(testNamespace).Get(t.Context(), "warm-conflict", metav1.GetOptions{})
		require.NoError(t, getErr)
		_, hasSession := lost.Labels[LabelSessionID]
		assert.False(t, hasSession)
	})

	t.Run("keeps oldest assigned pod and deletes the extra claim", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		sessionID := "session-dedupe"
		older := unassignedPod("warm-older", "10.0.0.1", time.Now().Add(-10*time.Minute))
		older.Labels[LabelSessionID] = sessionID
		newer := unassignedPod("warm-newer", "127.0.0.1", time.Now().Add(-time.Minute))
		pool := newTestPool(t, older, newer)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())
		ctx := t.Context()

		ip, name, err := pool.ClaimPod(ctx, sessionID)

		require.NoError(t, err)
		assert.Equal(t, "warm-older", name)
		assert.Equal(t, "10.0.0.1", ip)
		_, newerErr := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-newer", metav1.GetOptions{})
		assert.True(t, k8serrors.IsNotFound(newerErr), "extra claimed pod must be deleted")
		_, secretErr := pool.clientset.CoreV1().Secrets(testNamespace).Get(ctx, AuthSecretName(sessionID), metav1.GetOptions{})
		require.NoError(t, secretErr, "shared auth Secret must remain")
	})

	t.Run("keeps the claimed pod when it is oldest and deletes a newer extra", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		sessionID := "session-dedupe-claimed-oldest"
		older := unassignedPod("warm-claim-oldest", "127.0.0.1", time.Now().Add(-10*time.Minute))
		newer := unassignedPod("warm-extra-newer", "10.0.0.2", time.Now().Add(-time.Minute))
		newer.Labels[LabelSessionID] = sessionID
		pool := newTestPool(t, older, newer)
		pool.config.AgentPort = agentPortFromURL(t, ts.URL)
		pool.SetHTTPClient(ts.Client())
		ctx := t.Context()

		ip, name, err := pool.ClaimPod(ctx, sessionID)

		require.NoError(t, err)
		assert.Equal(t, "warm-claim-oldest", name)
		assert.Equal(t, "127.0.0.1", ip)
		_, extraErr := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-extra-newer", metav1.GetOptions{})
		assert.True(t, k8serrors.IsNotFound(extraErr), "newer extra pod must be deleted")
		kept, getErr := pool.clientset.CoreV1().Pods(testNamespace).Get(ctx, "warm-claim-oldest", metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.Equal(t, sessionID, kept.Labels[LabelSessionID])
		_, secretErr := pool.clientset.CoreV1().Secrets(testNamespace).Get(ctx, AuthSecretName(sessionID), metav1.GetOptions{})
		require.NoError(t, secretErr, "shared auth Secret must remain")
	})

	t.Run("breaks CreationTimestamp ties by pod name", func(t *testing.T) {
		sessionID := "session-dedupe-tie"
		created := metav1.NewTime(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
		aaa := unassignedPod("warm-aaa", "10.0.0.1", created.Time)
		aaa.Labels[LabelSessionID] = sessionID
		aaa.CreationTimestamp = created
		zzz := unassignedPod("warm-zzz", "127.0.0.1", created.Time)
		zzz.Labels[LabelSessionID] = sessionID
		zzz.CreationTimestamp = created

		client := fake.NewSimpleClientset(aaa.DeepCopy(), zzz.DeepCopy())
		pool := NewWarmPool(client, newTestConfig(), slog.Default())

		ip, name := pool.keepOldestAssigned(t.Context(), sessionID, "warm-zzz", "127.0.0.1")

		assert.Equal(t, "warm-aaa", name)
		assert.Equal(t, "10.0.0.1", ip)
		_, zzzErr := client.CoreV1().Pods(testNamespace).Get(t.Context(), "warm-zzz", metav1.GetOptions{})
		assert.True(t, k8serrors.IsNotFound(zzzErr), "lexicographically later name must be deleted on a timestamp tie")
		kept, getErr := client.CoreV1().Pods(testNamespace).Get(t.Context(), "warm-aaa", metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.Equal(t, sessionID, kept.Labels[LabelSessionID])
	})
}

func TestComparePodAge(t *testing.T) {
	t.Parallel()
	earlier := metav1.NewTime(time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC))
	later := metav1.NewTime(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	same := metav1.NewTime(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))

	older := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "z", CreationTimestamp: earlier}}
	newer := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", CreationTimestamp: later}}
	assert.Equal(t, -1, comparePodAge(older, newer))
	assert.Equal(t, 1, comparePodAge(newer, older))

	aaa := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "warm-aaa", CreationTimestamp: same}}
	zzz := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "warm-zzz", CreationTimestamp: same}}
	assert.Equal(t, -1, comparePodAge(aaa, zzz))
	assert.Equal(t, 1, comparePodAge(zzz, aaa))
	assert.Equal(t, 0, comparePodAge(aaa, aaa))
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

	t.Run("skips a terminating unassigned pod and claims the next ready one", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)

		dying := unassignedPod("warm-dying", "10.0.0.9", time.Now().Add(-10*time.Minute))
		del := metav1.Now()
		dying.DeletionTimestamp = &del
		dying.Finalizers = []string{"cli-mcp.redhat.com/test"}
		ready := unassignedPod("warm-next", "127.0.0.1", time.Now().Add(-time.Minute))

		client := fake.NewSimpleClientset()
		ctx := t.Context()
		_, err := client.CoreV1().Pods(testNamespace).Create(ctx, &dying, metav1.CreateOptions{})
		require.NoError(t, err)
		_, err = client.CoreV1().Pods(testNamespace).Create(ctx, &ready, metav1.CreateOptions{})
		require.NoError(t, err)

		cfg := newTestConfig()
		cfg.AgentPort = agentPortFromURL(t, ts.URL)
		mgr, err := NewSessionManager(client, cfg, slog.Default())
		require.NoError(t, err)
		mgr.Pool().SetHTTPClient(ts.Client())

		ip, getErr := mgr.GetOrCreatePod(ctx, "session-skip-dying")

		require.NoError(t, getErr)
		assert.Equal(t, "127.0.0.1", ip)
		claimed, claimedErr := client.CoreV1().Pods(testNamespace).Get(ctx, "warm-next", metav1.GetOptions{})
		require.NoError(t, claimedErr)
		assert.Equal(t, "session-skip-dying", claimed.Labels[LabelSessionID])
		skipped, skipErr := client.CoreV1().Pods(testNamespace).Get(ctx, "warm-dying", metav1.GetOptions{})
		require.NoError(t, skipErr)
		_, hasSession := skipped.Labels[LabelSessionID]
		assert.False(t, hasSession)
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
		_, secretErr := client.CoreV1().Secrets(testNamespace).Get(context.Background(), AuthSecretName("session-fallback"), metav1.GetOptions{})
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
		_, secretErr := client.CoreV1().Secrets(testNamespace).Get(context.Background(), AuthSecretName("session-claim-not-ready"), metav1.GetOptions{})
		require.NoError(t, secretErr, "auth secret should remain after request deadline")
	})
}
