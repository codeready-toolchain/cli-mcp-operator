package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNamespace = "tarsy"

func newTestConfig() SandboxConfig {
	cfg := DefaultConfig()
	cfg.HMACKey = "test-hmac-key"
	cfg.Image = "quay.io/codeready-toolchain/cli-mcp-sandbox:v0.1.0-test"
	cfg.Namespace = testNamespace
	return cfg
}

func newTestManager(t *testing.T, objects ...corev1.Pod) *SessionManager {
	t.Helper()
	client := fake.NewSimpleClientset()
	ctx := context.Background()
	for i := range objects {
		_, err := client.CoreV1().Pods(testNamespace).Create(ctx, &objects[i], metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return newTestManagerWithClient(t, client)
}

func newTestManagerWithClient(t *testing.T, client kubernetes.Interface) *SessionManager {
	t.Helper()
	mgr, err := NewSessionManager(client, newTestConfig(), slog.Default())
	require.NoError(t, err)
	return mgr
}

func readyPod(sessionID, ip string, createdAt time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podNamePrefix + sessionID,
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels: map[string]string{
				labelSessionID: sessionID,
				labelComponent: componentValue,
			},
			Annotations: map[string]string{
				annotationCreatedAt:    createdAt.Format(time.RFC3339),
				annotationLastActivity: createdAt.Format(time.RFC3339),
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

// agentPortFromURL extracts the numeric port from an httptest server URL.
func agentPortFromURL(t *testing.T, serverURL string) int {
	t.Helper()
	host := strings.TrimPrefix(serverURL, "http://")
	parts := strings.Split(host, ":")
	require.Len(t, parts, 2)
	port, err := strconv.Atoi(parts[1])
	require.NoError(t, err)
	return port
}

func TestValidateSessionID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"valid lowercase", "abc123", false},
		{"valid with hyphens", "inv-abc-123", false},
		{"single char", "a", false},
		{"max length 63", strings.Repeat("a", 63), false},
		{"too long 64 chars", strings.Repeat("a", 64), true},
		{"starts with hyphen", "-abc", true},
		{"ends with hyphen", "abc-", true},
		{"uppercase", "ABC", true},
		{"contains underscore", "abc_123", true},
		{"contains dot", "abc.123", true},
		{"empty string", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// when
			err := ValidateSessionID(tt.id)

			// then
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestComputeToken(t *testing.T) {
	t.Run("deterministic for same inputs", func(t *testing.T) {
		// when
		t1 := computeToken("key", "session-1")
		t2 := computeToken("key", "session-1")

		// then
		assert.Equal(t, t1, t2)
	})

	t.Run("different sessions produce different tokens", func(t *testing.T) {
		// when
		t1 := computeToken("key", "session-1")
		t2 := computeToken("key", "session-2")

		// then
		assert.NotEqual(t, t1, t2)
	})

	t.Run("different keys produce different tokens", func(t *testing.T) {
		// when
		t1 := computeToken("key-a", "session-1")
		t2 := computeToken("key-b", "session-1")

		// then
		assert.NotEqual(t, t1, t2)
	})

	t.Run("returns 64 hex chars", func(t *testing.T) {
		// when
		token := computeToken("key", "session-1")

		// then — SHA-256 = 32 bytes = 64 hex chars
		assert.Len(t, token, 64)
	})
}

func TestNewSessionManagerValidation(t *testing.T) {
	client := fake.NewSimpleClientset()

	t.Run("rejects empty HMACKey", func(t *testing.T) {
		cfg := newTestConfig()
		cfg.HMACKey = ""

		_, err := NewSessionManager(client, cfg, slog.Default())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "HMACKey")
	})

	t.Run("rejects empty Image", func(t *testing.T) {
		cfg := newTestConfig()
		cfg.Image = ""

		_, err := NewSessionManager(client, cfg, slog.Default())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "Image")
	})

	t.Run("accepts valid config", func(t *testing.T) {
		mgr, err := NewSessionManager(client, newTestConfig(), slog.Default())

		require.NoError(t, err)
		assert.NotNil(t, mgr)
	})
}

func TestGetOrCreatePod(t *testing.T) {
	t.Run("cache hit returns cached IP", func(t *testing.T) {
		// given
		mgr := newTestManager(t)
		mgr.cache.Set("inv-1", "10.0.0.1", "pod-inv-1")

		// when
		ip, err := mgr.GetOrCreatePod(context.Background(), "inv-1")

		// then
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.1", ip)
	})

	t.Run("discovers existing ready pod", func(t *testing.T) {
		// given
		pod := readyPod("inv-2", "10.0.0.2", time.Now().Add(-5*time.Minute))
		mgr := newTestManager(t, pod)

		// when
		ip, err := mgr.GetOrCreatePod(context.Background(), "inv-2")

		// then
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.2", ip)

		cachedIP, cachedName, ok := mgr.cache.Get("inv-2")
		assert.True(t, ok)
		assert.Equal(t, "10.0.0.2", cachedIP)
		assert.Equal(t, podNamePrefix+"inv-2", cachedName)
	})

	t.Run("waits for pending discovered pod to become ready", func(t *testing.T) {
		// given — labeled pod exists but is not Ready yet
		sessionID := "inv-pending"
		pod := readyPod(sessionID, "", time.Now())
		pod.Status.Phase = corev1.PodPending
		pod.Status.PodIP = ""
		pod.Status.Conditions = nil
		mgr := newTestManager(t, pod)
		ctx := context.Background()

		done := make(chan error, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			done <- markPodReady(mgr, sessionID, "10.0.0.42")
		}()

		// when
		ip, err := mgr.GetOrCreatePod(ctx, sessionID)

		// then
		require.NoError(t, <-done)
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.42", ip)
		cachedIP, cachedName, ok := mgr.cache.Get(sessionID)
		assert.True(t, ok)
		assert.Equal(t, "10.0.0.42", cachedIP)
		assert.Equal(t, podNamePrefix+sessionID, cachedName)
	})

	t.Run("selects oldest ready pod", func(t *testing.T) {
		// given
		older := readyPod("inv-3", "10.0.0.10", time.Now().Add(-10*time.Minute))
		older.Name = podNamePrefix + "inv-3-old"
		newer := readyPod("inv-3", "10.0.0.20", time.Now().Add(-1*time.Minute))
		newer.Name = podNamePrefix + "inv-3-new"
		mgr := newTestManager(t, older, newer)

		// when
		ip, err := mgr.GetOrCreatePod(context.Background(), "inv-3")

		// then
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.10", ip)
	})

	t.Run("rejects invalid session ID", func(t *testing.T) {
		// given
		mgr := newTestManager(t)

		// when
		_, err := mgr.GetOrCreatePod(context.Background(), "INVALID_ID")

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid session ID")
	})

	t.Run("creates pod on demand when cache empty", func(t *testing.T) {
		// given
		sessionID := "inv-ondemand"
		mgr := newTestManager(t)
		ctx := context.Background()
		done := make(chan error, 1)
		go func() { done <- markPodReady(mgr, sessionID, "10.0.0.99") }()

		// when
		ip, err := mgr.GetOrCreatePod(ctx, sessionID)

		// then
		require.NoError(t, <-done)
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.99", ip)
		cachedIP, _, ok := mgr.cache.Get(sessionID)
		assert.True(t, ok)
		assert.Equal(t, "10.0.0.99", cachedIP)
		_, secretErr := mgr.clientset.CoreV1().Secrets(testNamespace).Get(ctx, secretNamePrefix+sessionID, metav1.GetOptions{})
		require.NoError(t, secretErr)
	})

	t.Run("rediscovers when create returns AlreadyExists", func(t *testing.T) {
		// given
		sessionID := "inv-exists"
		existing := readyPod(sessionID, "10.0.0.77", time.Now().Add(-2*time.Minute))
		client := fake.NewSimpleClientset()
		client.PrependReactor("create", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			require.NoError(t, client.Tracker().Add(existing.DeepCopy()))
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, existing.Name)
		})
		mgr := newTestManagerWithClient(t, client)

		// when
		ip, err := mgr.GetOrCreatePod(context.Background(), sessionID)

		// then
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.77", ip)
		cachedIP, _, ok := mgr.cache.Get(sessionID)
		assert.True(t, ok)
		assert.Equal(t, "10.0.0.77", cachedIP)
	})
}

// markPodReady updates a newly created sandbox pod to Running/Ready so waitForReady can succeed.
func markPodReady(mgr *SessionManager, sessionID, ip string) error {
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pod, err := mgr.clientset.CoreV1().Pods(testNamespace).Get(ctx, podNamePrefix+sessionID, metav1.GetOptions{})
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = ip
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}
		_, err = mgr.clientset.CoreV1().Pods(testNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		return err
	}
	return fmt.Errorf("pod %s not found before deadline", podNamePrefix+sessionID)
}

func TestBuildPodSpec(t *testing.T) {
	// given
	mgr := newTestManager(t)

	// when
	pod := mgr.buildPodSpec("inv-spec")

	// then — name and namespace
	assert.Equal(t, podNamePrefix+"inv-spec", pod.Name)
	assert.Equal(t, testNamespace, pod.Namespace)

	// then — labels
	assert.Equal(t, "inv-spec", pod.Labels[labelSessionID])
	assert.Equal(t, componentValue, pod.Labels[labelComponent])

	// then — annotations
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

	// then — readiness probe (exec curl /health on loopback; works with server-only NetworkPolicy)
	require.NotNil(t, container.ReadinessProbe)
	require.NotNil(t, container.ReadinessProbe.Exec)
	require.Equal(t, []string{
		"curl",
		"-fsS",
		"--max-time",
		"1",
		"http://127.0.0.1:8090/health",
	}, container.ReadinessProbe.Exec.Command)
	assert.Nil(t, container.ReadinessProbe.HTTPGet)
	assert.Equal(t, int32(2), container.ReadinessProbe.InitialDelaySeconds)
	assert.Equal(t, int32(2), container.ReadinessProbe.TimeoutSeconds)
	assert.Equal(t, int32(10), container.ReadinessProbe.PeriodSeconds)

	// then — volumes and mounts
	assert.Len(t, pod.Spec.Volumes, 2)
	assert.Len(t, container.VolumeMounts, 2)

	// then — env vars
	envNames := make(map[string]bool)
	for _, e := range container.Env {
		envNames[e.Name] = true
	}
	assert.True(t, envNames["KUBECONFIG"])
	assert.True(t, envNames["HOME"])
	assert.True(t, envNames["SANDBOX_AUTH_TOKEN"])
}

func TestBuildAuthSecret(t *testing.T) {
	// given
	mgr := newTestManager(t)
	token := computeToken(mgr.config.HMACKey, "inv-sec")

	// when
	secret := buildAuthSecret(testNamespace, "inv-sec", token)

	// then
	assert.Equal(t, secretNamePrefix+"inv-sec", secret.Name)
	assert.Equal(t, testNamespace, secret.Namespace)
	assert.Equal(t, "inv-sec", secret.Labels[labelSessionID])
	assert.Equal(t, componentValue, secret.Labels[labelComponent])
	assert.Equal(t, token, secret.StringData["token"])
}

func TestShouldCleanupAfterWaitFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped canceled", fmt.Errorf("wait: %w", context.Canceled), false},
		{"wrapped deadline", fmt.Errorf("wait: %w", context.DeadlineExceeded), false},
		{"ready timeout", fmt.Errorf("pod %q not ready within %s", "p", readyTimeout), true},
		{"other error", errors.New("get pod failed"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldCleanupAfterWaitFailure(tt.err))
		})
	}
}

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name  string
		pod   corev1.Pod
		ready bool
	}{
		{
			"running with PodReady true",
			corev1.Pod{Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			}},
			true,
		},
		{
			"running with PodReady false",
			corev1.Pod{Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			}},
			false,
		},
		{
			"pending phase",
			corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			false,
		},
		{
			"running without conditions",
			corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// when
			result := isPodReady(&tt.pod)

			// then
			assert.Equal(t, tt.ready, result)
		})
	}
}

func TestCleanupSession(t *testing.T) {
	// given
	pod := readyPod("inv-clean", "10.0.0.5", time.Now())
	mgr := newTestManager(t, pod)
	mgr.cache.Set("inv-clean", "10.0.0.5", podNamePrefix+"inv-clean")

	ctx := context.Background()
	secret := buildAuthSecret(testNamespace, "inv-clean", "tok")
	_, err := mgr.clientset.CoreV1().Secrets(testNamespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err)

	// when
	err = mgr.CleanupSession(ctx, "inv-clean")

	// then
	require.NoError(t, err)

	_, _, ok := mgr.cache.Get("inv-clean")
	assert.False(t, ok, "cache entry should be cleared")

	pods, err := mgr.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", labelSessionID, "inv-clean"),
	})
	require.NoError(t, err)
	assert.Empty(t, pods.Items, "pods should be deleted")

	_, err = mgr.clientset.CoreV1().Secrets(testNamespace).Get(ctx, secretNamePrefix+"inv-clean", metav1.GetOptions{})
	assert.Error(t, err, "secret should be deleted")
}

func TestCleanupStale(t *testing.T) {
	t.Run("stale pod is cleaned up", func(t *testing.T) {
		// given
		staleTime := time.Now().Add(-2 * time.Hour)
		stalePod := readyPod("stale-1", "10.0.0.1", staleTime)
		mgr := newTestManager(t, stalePod)

		// when
		cleaned, err := mgr.CleanupStale(context.Background())

		// then
		require.NoError(t, err)
		assert.Equal(t, 1, cleaned)
	})

	t.Run("fresh pod is preserved", func(t *testing.T) {
		// given
		freshPod := readyPod("fresh-1", "10.0.0.2", time.Now())
		mgr := newTestManager(t, freshPod)

		// when
		cleaned, err := mgr.CleanupStale(context.Background())

		// then
		require.NoError(t, err)
		assert.Equal(t, 0, cleaned)

		pods, err := mgr.clientset.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, pods.Items, 1)
	})

	t.Run("falls back to created-at when last-activity is missing", func(t *testing.T) {
		// given
		staleTime := time.Now().Add(-2 * time.Hour)
		pod := readyPod("no-activity", "10.0.0.3", staleTime)
		delete(pod.Annotations, annotationLastActivity)
		mgr := newTestManager(t, pod)

		// when
		cleaned, err := mgr.CleanupStale(context.Background())

		// then
		require.NoError(t, err)
		assert.Equal(t, 1, cleaned)
	})
}

func TestExecuteCommand(t *testing.T) {
	t.Run("proxies command and returns response", func(t *testing.T) {
		// given
		expectedResp := agent.ExecResponse{
			Stdout:     "hello world",
			Stderr:     "",
			ExitCode:   0,
			DurationMs: 42,
		}

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/exec", r.URL.Path)
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "))

			var req agent.ExecRequest
			if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
				t.Errorf("decode request: %v", decodeErr)
				return
			}
			assert.Equal(t, "echo hello", req.Command)
			assert.Equal(t, 60, req.Timeout)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(expectedResp)
		}))
		t.Cleanup(ts.Close)

		mgr := newTestManager(t)
		mgr.config.AgentPort = agentPortFromURL(t, ts.URL)
		mgr.SetHTTPClient(ts.Client())
		mgr.cache.Set("inv-exec", "127.0.0.1", "pod-exec")

		// when
		resp, err := mgr.ExecuteCommand(context.Background(), "inv-exec", "echo hello", 60)

		// then
		require.NoError(t, err)
		assert.Equal(t, "hello world", resp.Stdout)
		assert.Equal(t, 0, resp.ExitCode)
		assert.Equal(t, int64(42), resp.DurationMs)
	})

	t.Run("sends correct HMAC token", func(t *testing.T) {
		// given
		cfg := newTestConfig()
		expectedToken := computeToken(cfg.HMACKey, "inv-token")

		var receivedToken string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(agent.ExecResponse{})
		}))
		t.Cleanup(ts.Close)

		mgr := newTestManager(t)
		mgr.config.AgentPort = agentPortFromURL(t, ts.URL)
		mgr.SetHTTPClient(ts.Client())
		mgr.cache.Set("inv-token", "127.0.0.1", "pod-token")

		// when
		_, err := mgr.ExecuteCommand(context.Background(), "inv-token", "ls", 30)

		// then
		require.NoError(t, err)
		assert.Equal(t, expectedToken, receivedToken)
	})

	t.Run("transport failure invalidates cache", func(t *testing.T) {
		// given — cache points at localhost; AgentPort has nothing listening
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		closedPort := ln.Addr().(*net.TCPAddr).Port
		require.NoError(t, ln.Close())

		mgr := newTestManager(t)
		mgr.config.AgentPort = closedPort
		mgr.SetHTTPClient(&http.Client{Timeout: 200 * time.Millisecond})
		mgr.cache.Set("inv-transport", "127.0.0.1", "pod-transport")

		// when
		_, execErr := mgr.ExecuteCommand(context.Background(), "inv-transport", "echo hi", 5)

		// then
		require.Error(t, execErr)
		_, _, ok := mgr.cache.Get("inv-transport")
		assert.False(t, ok, "cache entry should be invalidated on transport error")
	})

	t.Run("updates last-activity even if cache expires during Execute", func(t *testing.T) {
		// given — expire the cache while the agent request is in flight
		createdAt := time.Now().UTC().Add(-time.Minute)
		pod := readyPod("inv-activity", "127.0.0.1", createdAt)
		oldActivity := createdAt.Format(time.RFC3339)

		started := make(chan struct{})
		release := make(chan struct{})
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(agent.ExecResponse{Stdout: "ok", ExitCode: 0})
		}))
		t.Cleanup(ts.Close)

		mgr := newTestManager(t, pod)
		mgr.config.AgentPort = agentPortFromURL(t, ts.URL)
		mgr.SetHTTPClient(ts.Client())
		mgr.cache.Set("inv-activity", "127.0.0.1", pod.Name)

		errCh := make(chan error, 1)
		go func() {
			_, err := mgr.ExecuteCommand(context.Background(), "inv-activity", "sleep 1", 60)
			errCh <- err
		}()

		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("agent request did not start")
		}
		mgr.cache.Delete("inv-activity")
		close(release)

		// when / then
		require.NoError(t, <-errCh)
		require.Eventually(t, func() bool {
			updated, getErr := mgr.clientset.CoreV1().Pods(testNamespace).Get(
				context.Background(), pod.Name, metav1.GetOptions{},
			)
			if getErr != nil {
				return false
			}
			return updated.Annotations[annotationLastActivity] != oldActivity
		}, time.Second, 10*time.Millisecond, "last-activity should be patched using pre-Execute podName")
	})
}
