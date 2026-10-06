package session

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/agent"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	podNamePrefix = "cli-mcp-sandbox-"

	readyPollPeriod = 2 * time.Second
	readyTimeout    = 60 * time.Second
	proxyGateTTL    = 5 * time.Second
	proxyGateFlight = "ready"
)

var sessionIDRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// SessionManager handles sandbox pod lifecycle: discovery, creation,
// command proxying, and cleanup.
//
//nolint:revive // name matches design docs
type SessionManager struct {
	clientset    kubernetes.Interface
	config       SandboxConfig
	cache        *PodCache
	pool         *WarmPool
	agentClient  *agent.AgentClient
	logger       *slog.Logger
	proxyGate    proxyGateCache
	proxyGateTTL time.Duration
}

type proxyGateCache struct {
	mu     sync.Mutex
	set    bool
	at     time.Time
	err    error
	flight singleflight.Group
}

// NewSessionManager creates a SessionManager with a default PodCache, agent
// client, and claim helper. Claim is always available; the MCP process does
// not replenish a warm pool or idle-GC assigned pods.
func NewSessionManager(clientset kubernetes.Interface, config SandboxConfig, logger *slog.Logger) (*SessionManager, error) {
	if config.HMACKey == "" {
		return nil, fmt.Errorf("SandboxConfig.HMACKey must not be empty")
	}
	if config.Image == "" {
		return nil, fmt.Errorf("SandboxConfig.Image must not be empty")
	}
	if config.InstanceName == "" {
		return nil, fmt.Errorf("SandboxConfig.InstanceName must not be empty")
	}
	if config.Namespace == "" {
		return nil, fmt.Errorf("SandboxConfig.Namespace must not be empty")
	}
	if config.ServiceAccountName == "" {
		return nil, fmt.Errorf("SandboxConfig.ServiceAccountName must not be empty")
	}
	if err := validateSandboxIdentity(config); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	config.AgentPort = cmp.Or(config.AgentPort, DefaultConfig().AgentPort)
	mgr := &SessionManager{
		clientset: clientset,
		config:    config,
		cache:     NewPodCache(defaultCacheTTL),
		pool:      NewWarmPool(clientset, config, logger),
		// Timeout 0: command duration is bounded by the ExecRequest timeout, not the HTTP client.
		agentClient: agent.NewAgentClient(
			agent.WithPort(config.AgentPort),
			agent.WithTimeout(0),
		),
		logger:       logger,
		proxyGateTTL: proxyGateTTL,
	}
	return mgr, nil
}

func validateSandboxIdentity(config SandboxConfig) error {
	proxy := config.ProxyService != "" || config.ProxyCASecret != "" || config.DummyKubeconfigConfigMap != ""
	if proxy {
		if config.ProxyService == "" || config.ProxyCASecret == "" {
			return fmt.Errorf("SandboxConfig.ProxyService and ProxyCASecret are required together")
		}
		if config.KubeconfigSecret != "" {
			return fmt.Errorf("SandboxConfig.KubeconfigSecret must be empty when proxy flags are set")
		}
		return nil
	}
	if config.KubeconfigSecret == "" {
		return fmt.Errorf("SandboxConfig.KubeconfigSecret must not be empty")
	}
	return nil
}

// Pool returns the claim helper. Exposed for testing.
func (m *SessionManager) Pool() *WarmPool {
	return m.pool
}

// SetHTTPClient replaces the agent client's underlying HTTP client (useful for testing).
// The provided client's timeout is preserved (do not override with WithTimeout).
func (m *SessionManager) SetHTTPClient(c *http.Client) {
	m.agentClient = agent.NewAgentClient(
		agent.WithHTTPClient(c),
		agent.WithPort(m.config.AgentPort),
	)
}

// SetCache replaces the default PodCache (useful for testing).
func (m *SessionManager) SetCache(c *PodCache) {
	m.cache = c
}

// ValidateSessionID checks that sessionID matches RFC 1123 DNS label format.
func ValidateSessionID(sessionID string) error {
	if !sessionIDRegex.MatchString(sessionID) {
		return fmt.Errorf("invalid session ID %q: must match RFC 1123 DNS label format", sessionID)
	}
	return nil
}

// ensureProxyReady lists this proxy Service's EndpointSlices and GETs the sandbox NetworkPolicy.
// Already-assigned sessions do not call this. Success and other lookup failures
// are cached for a short TTL. context.Canceled and context.DeadlineExceeded are
// not cached. The mutex covers only that cache; concurrent callers share one lookup.
func (m *SessionManager) ensureProxyReady(ctx context.Context) error {
	if m.config.ProxyService == "" {
		return nil
	}
	if ok, gateErr := m.cachedProxyGate(); ok {
		return gateErr
	}
	ch := m.proxyGate.flight.DoChan(proxyGateFlight, func() (any, error) {
		if ok, gateErr := m.cachedProxyGate(); ok {
			return gateErr, nil
		}
		lookupErr := m.lookupProxyReady(ctx)
		if proxyGateCacheable(lookupErr) {
			m.storeProxyGate(lookupErr)
		}
		return lookupErr, nil
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return res.Err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		gateErr, _ := res.Val.(error)
		return gateErr
	}
}

func (m *SessionManager) cachedProxyGate() (bool, error) {
	m.proxyGate.mu.Lock()
	defer m.proxyGate.mu.Unlock()
	if !m.proxyGate.set || time.Since(m.proxyGate.at) >= m.proxyGateTTL {
		return false, nil
	}
	return true, m.proxyGate.err
}

func (m *SessionManager) storeProxyGate(err error) {
	m.proxyGate.mu.Lock()
	defer m.proxyGate.mu.Unlock()
	m.proxyGate.set = true
	m.proxyGate.at = time.Now()
	m.proxyGate.err = err
}

func proxyGateCacheable(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (m *SessionManager) lookupProxyReady(ctx context.Context) error {
	selector := labels.Set{discoveryv1.LabelServiceName: m.config.ProxyService}.AsSelector().String()
	sliceList, err := m.clientset.DiscoveryV1().EndpointSlices(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return fmt.Errorf("list proxy endpoint slices: %w", err)
	}
	if !EndpointSlicesReady(sliceList.Items) {
		return fmt.Errorf("proxy service %s has no ready endpoint addresses", m.config.ProxyService)
	}
	npName := SandboxNetworkPolicyName(m.config.InstanceName)
	np, err := m.clientset.NetworkingV1().NetworkPolicies(m.config.Namespace).Get(ctx, npName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get sandbox network policy: %w", err)
	}
	if !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		return fmt.Errorf("sandbox network policy %s does not set Egress", npName)
	}
	return nil
}

// EndpointSlicesReady reports whether any slice has an address that can receive
// traffic. A nil Ready condition counts as ready, matching EndpointSlice compatibility.
func EndpointSlicesReady(endpointSlices []discoveryv1.EndpointSlice) bool {
	for i := range endpointSlices {
		for _, ep := range endpointSlices[i].Endpoints {
			if len(ep.Addresses) == 0 {
				continue
			}
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return true
			}
		}
	}
	return false
}

// GetOrCreatePod resolves or creates a sandbox pod for the session.
// Lookup order: cache → label-based K8s API discovery → claim unassigned → on-demand create.
func (m *SessionManager) GetOrCreatePod(ctx context.Context, sessionID string) (podIP string, err error) {
	if validationErr := ValidateSessionID(sessionID); validationErr != nil {
		return "", validationErr
	}

	if ip, _, ok := m.cache.Get(sessionID); ok {
		return ip, nil
	}

	ip, podName, err := m.discoverPod(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("discover pod: %w", err)
	}
	if ip != "" {
		m.cache.Set(sessionID, ip, podName)
		return ip, nil
	}

	if gateErr := m.ensureProxyReady(ctx); gateErr != nil {
		return "", gateErr
	}

	claimedIP, claimedPodName, claimErr := m.pool.ClaimPod(ctx, sessionID)
	if claimErr == nil {
		return m.waitAndCacheClaim(ctx, sessionID, claimedPodName, claimedIP)
	}
	m.logger.Debug("no unassigned pod claimed, creating on demand", "session", sessionID, "error", claimErr)

	ip, podName, err = m.discoverPod(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("discover pod after claim: %w", err)
	}
	if ip != "" {
		m.cache.Set(sessionID, ip, podName)
		return ip, nil
	}

	ip, podName, err = m.createSandboxPod(ctx, sessionID)
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			ip, podName, err = m.discoverPod(ctx, sessionID)
			if err != nil {
				return "", fmt.Errorf("discover pod after conflict: %w", err)
			}
			if ip != "" {
				m.cache.Set(sessionID, ip, podName)
				return ip, nil
			}
			return "", fmt.Errorf("pod exists but not discoverable for session %q", sessionID)
		}
		return "", fmt.Errorf("create sandbox pod: %w", err)
	}
	m.cache.Set(sessionID, ip, podName)
	return ip, nil
}

// waitAndCacheClaim waits for a claimed pool pod to become Ready. If that pod
// disappears because a sibling kept an older assigned pod, rediscover and do
// not delete the shared auth Secret.
func (m *SessionManager) waitAndCacheClaim(ctx context.Context, sessionID, podName, fallbackIP string) (string, error) {
	readyIP, readyErr := m.waitForReady(ctx, podName)
	if readyErr == nil {
		if readyIP == "" {
			readyIP = fallbackIP
		}
		m.cache.Set(sessionID, readyIP, podName)
		return readyIP, nil
	}
	if !shouldCleanupAfterWaitFailure(readyErr) {
		return "", fmt.Errorf("claimed pod not ready: %w", readyErr)
	}
	ip, other, found, listErr := m.lookupAssignedSibling(ctx, sessionID, podName)
	if listErr != nil {
		return "", fmt.Errorf("claimed pod not ready: %w", errors.Join(readyErr, listErr))
	}
	if found {
		if other != "" && ip != "" {
			m.cache.Set(sessionID, ip, other)
			return ip, nil
		}
		return "", fmt.Errorf("claimed pod not ready: %w", readyErr)
	}
	m.bestEffortCleanupFailedPod(podName, sessionID)
	return "", fmt.Errorf("claimed pod not ready: %w", readyErr)
}

// lookupAssignedSibling is a non-waiting list of assigned pods for sessionID,
// excluding excludeName. A list error must not be treated as "no sibling".
func (m *SessionManager) lookupAssignedSibling(ctx context.Context, sessionID, excludeName string) (ip, name string, found bool, err error) {
	pods, err := m.clientset.CoreV1().Pods(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: AssignedSelector(m.config.InstanceName, sessionID),
	})
	if err != nil {
		return "", "", false, fmt.Errorf("list assigned pods: %w", err)
	}
	var ready []corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Name == excludeName || p.DeletionTimestamp != nil || isTerminalPod(p) {
			continue
		}
		found = true
		if isPodReady(p) {
			ready = append(ready, *p)
		}
	}
	if len(ready) == 0 {
		return "", "", found, nil
	}
	slices.SortFunc(ready, comparePodAge)
	oldest := ready[0]
	return oldest.Status.PodIP, oldest.Name, true, nil
}

// discoverPod lists pods by label selector and returns the oldest Ready pod's IP.
// If no Ready pod exists but a non-terminal pod is found, it waits for readiness.
func (m *SessionManager) discoverPod(ctx context.Context, sessionID string) (podIP, podName string, err error) {
	pods, err := m.clientset.CoreV1().Pods(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: AssignedSelector(m.config.InstanceName, sessionID),
	})
	if err != nil {
		return "", "", fmt.Errorf("list pods: %w", err)
	}

	var readyPods []corev1.Pod
	var pendingPod *corev1.Pod

	for i := range pods.Items {
		p := &pods.Items[i]
		phase := p.Status.Phase
		if phase == corev1.PodFailed || phase == corev1.PodSucceeded {
			continue
		}
		if isPodReady(p) {
			readyPods = append(readyPods, *p)
		} else if pendingPod == nil {
			pendingPod = p
		}
	}

	if len(readyPods) > 0 {
		sort.Slice(readyPods, func(i, j int) bool {
			return readyPods[i].CreationTimestamp.Before(&readyPods[j].CreationTimestamp)
		})
		oldest := readyPods[0]
		return oldest.Status.PodIP, oldest.Name, nil
	}

	if pendingPod != nil {
		ip, err := m.waitForReady(ctx, pendingPod.Name)
		if err != nil {
			return "", "", err
		}
		return ip, pendingPod.Name, nil
	}

	return "", "", nil
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// buildPodSpec constructs a session-specific pod by applying session fields
// (session-id label, SANDBOX_AUTH_TOKEN env) on top of the shared base pod spec.
func (m *SessionManager) buildPodSpec(sessionID string) *corev1.Pod {
	pod := BuildBasePodSpec(podNamePrefix+sessionID, m.config)
	pod.Labels[LabelSessionID] = sessionID
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
		Name: "SANDBOX_AUTH_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: AuthSecretName(sessionID),
				},
				Key: "token",
			},
		},
	})
	return pod
}

// buildAuthSecret constructs the per-session auth Secret containing the
// HMAC-derived token. Shared by SessionManager and WarmPool.
func buildAuthSecret(namespace, instance, sessionID, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AuthSecretName(sessionID),
			Namespace: namespace,
			Labels: map[string]string{
				LabelSessionID: sessionID,
				LabelComponent: ComponentSandbox,
				LabelInstance:  instance,
			},
		},
		StringData: map[string]string{
			"token": token,
		},
	}
}

func computeToken(hmacKey, sessionID string) string {
	mac := hmac.New(sha256.New, []byte(hmacKey))
	mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *SessionManager) waitForReady(ctx context.Context, podName string) (string, error) {
	deadline := time.After(readyTimeout)
	ticker := time.NewTicker(readyPollPeriod)
	defer ticker.Stop()

	check := func() (string, bool, error) {
		pod, err := m.clientset.CoreV1().Pods(m.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return "", false, fmt.Errorf("get pod %q: %w", podName, err)
		}
		if isPodReady(pod) {
			return pod.Status.PodIP, true, nil
		}
		return "", false, nil
	}

	if ip, ready, err := check(); err != nil {
		return "", err
	} else if ready {
		return ip, nil
	}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("pod %q not ready within %s", podName, readyTimeout)
		case <-ticker.C:
			ip, ready, err := check()
			if err != nil {
				return "", err
			}
			if ready {
				return ip, nil
			}
		}
	}
}

func (m *SessionManager) createSandboxPod(ctx context.Context, sessionID string) (podIP, podName string, err error) {
	token := computeToken(m.config.HMACKey, sessionID)

	secret := buildAuthSecret(m.config.Namespace, m.config.InstanceName, sessionID, token)
	_, err = m.clientset.CoreV1().Secrets(m.config.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return "", "", fmt.Errorf("create auth secret: %w", err)
	}

	pod := m.buildPodSpec(sessionID)
	created, err := m.clientset.CoreV1().Pods(m.config.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return "", "", err
		}
		bestEffortDeleteSecret(ctx, m.clientset, m.config.Namespace, sessionID, m.logger)
		return "", "", fmt.Errorf("create pod: %w", err)
	}

	ip, waitErr := m.waitForReady(ctx, created.Name)
	if waitErr != nil {
		// Delete only when the pod failed our ready wait. If the request context
		// was canceled or timed out, another replica may still be waiting on the
		// same pod via discoverPod — leave resources for a sibling waiter.
		if shouldCleanupAfterWaitFailure(waitErr) {
			m.bestEffortCleanupFailedPod(created.Name, sessionID)
		}
		return "", "", fmt.Errorf("wait for pod ready: %w", waitErr)
	}
	return ip, created.Name, nil
}

// shouldCleanupAfterWaitFailure reports whether a waitForReady error means the
// pod itself failed to become Ready (cleanup), versus the caller giving up
// (Canceled/DeadlineExceeded — do not delete; sibling replicas may still wait).
func shouldCleanupAfterWaitFailure(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// bestEffortCleanupFailedPod deletes a pod and its auth Secret using a short-lived
// context independent of the request (which may already be canceled or timed out).
func (m *SessionManager) bestEffortCleanupFailedPod(podName, sessionID string) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	bestEffortDeletePod(cleanupCtx, m.clientset, m.config.Namespace, podName, m.logger)
	bestEffortDeleteSecret(cleanupCtx, m.clientset, m.config.Namespace, sessionID, m.logger)
}

// bestEffortDeletePod deletes a sandbox pod, logging a warning on failure.
func bestEffortDeletePod(ctx context.Context, clientset kubernetes.Interface, namespace, podName string, logger *slog.Logger) {
	err := clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		logger.Warn("failed to delete sandbox pod", "pod", podName, "error", err)
	}
}

// bestEffortDeleteSecret deletes the per-session auth Secret, logging a
// warning on failure. Shared by SessionManager and WarmPool.
func bestEffortDeleteSecret(ctx context.Context, clientset kubernetes.Interface, namespace, sessionID string, logger *slog.Logger) {
	secretName := AuthSecretName(sessionID)
	err := clientset.CoreV1().Secrets(namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		logger.Warn("failed to delete auth secret", "secret", secretName, "error", err)
	}
}

// ExecuteCommand resolves the pod for the session and proxies the command to the agent.
func (m *SessionManager) ExecuteCommand(ctx context.Context, sessionID, command string, timeoutSec int) (*agent.ExecResponse, error) {
	podIP, err := m.GetOrCreatePod(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("resolve pod: %w", err)
	}

	token := computeToken(m.config.HMACKey, sessionID)
	// Capture podName before Execute: cache TTL can expire during long-running commands.
	_, podName, _ := m.cache.Get(sessionID)
	execResp, err := m.agentClient.Execute(ctx, podIP, token, agent.ExecRequest{
		Command: command,
		Timeout: timeoutSec,
	})
	if err != nil {
		var netErr *agent.NetworkError
		if errors.As(err, &netErr) {
			m.cache.Invalidate(sessionID, podIP)
		}
		return nil, fmt.Errorf("agent request: %w", err)
	}

	if podName != "" {
		// WithoutCancel: keep the request-derived context for gosec G118, but do not
		// cancel the annotation patch when the caller context ends.
		go m.updateLastActivity(context.WithoutCancel(ctx), podName)
	}

	return execResp, nil
}

func (m *SessionManager) updateLastActivity(ctx context.Context, podName string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	now := time.Now().UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AnnotationLastActivity, now)

	_, err := m.clientset.CoreV1().Pods(m.config.Namespace).Patch(
		ctx, podName, types.MergePatchType, []byte(patch), metav1.PatchOptions{},
	)
	if err != nil {
		m.logger.Warn("failed to update last-activity annotation", "pod", podName, "error", err)
	}
}

// CleanupSession deletes all resources for a session: pods, auth Secret, and cache entry.
func (m *SessionManager) CleanupSession(ctx context.Context, sessionID string) error {
	m.cache.Delete(sessionID)

	pods, err := m.clientset.CoreV1().Pods(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: AssignedSelector(m.config.InstanceName, sessionID),
	})
	if err != nil {
		return fmt.Errorf("list pods for cleanup: %w", err)
	}

	var errs []error
	for i := range pods.Items {
		if delErr := m.clientset.CoreV1().Pods(m.config.Namespace).Delete(ctx, pods.Items[i].Name, metav1.DeleteOptions{}); delErr != nil && !k8serrors.IsNotFound(delErr) {
			errs = append(errs, fmt.Errorf("delete pod %s: %w", pods.Items[i].Name, delErr))
		}
	}

	secretName := AuthSecretName(sessionID)
	if delErr := m.clientset.CoreV1().Secrets(m.config.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); delErr != nil && !k8serrors.IsNotFound(delErr) {
		errs = append(errs, fmt.Errorf("delete secret %s: %w", secretName, delErr))
	}

	return errors.Join(errs...)
}
