package session

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/agent"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const (
	labelSessionID = "tarsy.redhat.com/session-id"
	labelComponent = "tarsy.redhat.com/component"
	componentValue = "cli-mcp-sandbox"

	annotationCreatedAt    = "tarsy.redhat.com/created-at"
	annotationLastActivity = "tarsy.redhat.com/last-activity"

	podNamePrefix    = "cli-mcp-sandbox-"
	secretNamePrefix = "cli-mcp-sandbox-auth-"

	readyPollPeriod = 2 * time.Second
	readyTimeout    = 60 * time.Second
)

var sessionIDRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// SessionManager handles sandbox pod lifecycle: discovery, creation,
// command proxying, and cleanup.
type SessionManager struct {
	clientset   kubernetes.Interface
	config      SandboxConfig
	cache       *PodCache
	pool        *WarmPool
	agentClient *agent.AgentClient
	logger      *slog.Logger
}

// NewSessionManager creates a SessionManager with a default PodCache and agent client.
// It returns an error if required config fields (HMACKey, Image) are missing.
func NewSessionManager(clientset kubernetes.Interface, config SandboxConfig, logger *slog.Logger) (*SessionManager, error) {
	if config.HMACKey == "" {
		return nil, fmt.Errorf("SandboxConfig.HMACKey must not be empty")
	}
	if config.Image == "" {
		return nil, fmt.Errorf("SandboxConfig.Image must not be empty")
	}
	if logger == nil {
		logger = slog.Default()
	}
	mgr := &SessionManager{
		clientset: clientset,
		config:    config,
		cache:     NewPodCache(defaultCacheTTL),
		// Timeout 0: command duration is bounded by the ExecRequest timeout, not the HTTP client.
		agentClient: agent.NewAgentClient(
			agent.WithPort(config.AgentPort),
			agent.WithTimeout(0),
		),
		logger: logger,
	}
	if config.WarmPoolSize > 0 {
		mgr.pool = NewWarmPool(clientset, config, logger)
	}
	return mgr, nil
}

// StartPool starts the warm pool reconciler if the pool is enabled.
// It should be called once after NewSessionManager during server startup.
func (m *SessionManager) StartPool(ctx context.Context) {
	if m.pool != nil {
		m.pool.StartReconciler(ctx)
	}
}

// Pool returns the warm pool (nil when disabled). Exposed for testing.
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

// GetOrCreatePod resolves or creates a sandbox pod for the session.
// Lookup order: cache → label-based K8s API discovery → idempotent create.
func (m *SessionManager) GetOrCreatePod(ctx context.Context, sessionID string) (podIP string, err error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
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

	if m.pool != nil {
		ip, podName, claimErr := m.pool.ClaimPod(ctx, sessionID)
		if claimErr == nil {
			m.cache.Set(sessionID, ip, podName)
			return ip, nil
		}
		m.logger.Info("warm pool claim failed, falling back to on-demand creation", "session", sessionID, "error", claimErr)
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

// discoverPod lists pods by label selector and returns the oldest Ready pod's IP.
// If no Ready pod exists but a non-terminal pod is found, it waits for readiness.
func (m *SessionManager) discoverPod(ctx context.Context, sessionID string) (podIP, podName string, err error) {
	selector := fmt.Sprintf("%s=%s,%s=%s", labelSessionID, sessionID, labelComponent, componentValue)
	pods, err := m.clientset.CoreV1().Pods(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
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

// buildBasePodSpec constructs the shared sandbox pod spec used by both
// on-demand creation and warm pool pre-creation. It includes the pod name,
// component label, created-at and last-activity annotations, security context,
// readiness probe, volumes, and base env vars. Callers add session-specific
// fields (session-id label, SANDBOX_AUTH_TOKEN env var) as needed.
func buildBasePodSpec(name string, config SandboxConfig) *corev1.Pod {
	now := time.Now().UTC().Format(time.RFC3339)
	runAsNonRoot := true
	var runAsUser int64 = 1001
	var runAsGroup int64 = 1001
	allowPrivEsc := false

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.Namespace,
			Labels: map[string]string{
				labelComponent: componentValue,
			},
			Annotations: map[string]string{
				annotationCreatedAt:    now,
				annotationLastActivity: now,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: config.ServiceAccountName,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				RunAsUser:    &runAsUser,
				RunAsGroup:   &runAsGroup,
			},
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: config.Image,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(config.CPURequest),
							corev1.ResourceMemory: resource.MustParse(config.MemoryRequest),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(config.CPULimit),
							corev1.ResourceMemory: resource.MustParse(config.MemoryLimit),
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromInt32(int32(config.AgentPort)),
							},
						},
						InitialDelaySeconds: 2,
						PeriodSeconds:       10,
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivEsc,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					Env: []corev1.EnvVar{
						{Name: "KUBECONFIG", Value: "/config/kubeconfig"},
						{Name: "HOME", Value: "/workspace"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "kubeconfig", MountPath: "/config", ReadOnly: true},
						{Name: "workspace", MountPath: "/workspace"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "kubeconfig",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: config.KubeconfigSecret,
						},
					},
				},
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}
}

// buildPodSpec constructs a session-specific pod by applying session fields
// (session-id label, SANDBOX_AUTH_TOKEN env) on top of the shared base pod spec.
func (m *SessionManager) buildPodSpec(sessionID string) *corev1.Pod {
	pod := buildBasePodSpec(podNamePrefix+sessionID, m.config)
	pod.Labels[labelSessionID] = sessionID
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
		Name: "SANDBOX_AUTH_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: secretNamePrefix + sessionID,
				},
				Key: "token",
			},
		},
	})
	return pod
}

// buildAuthSecret constructs the per-session auth Secret containing the
// HMAC-derived token. Shared by SessionManager and WarmPool.
func buildAuthSecret(namespace, sessionID, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretNamePrefix + sessionID,
			Namespace: namespace,
			Labels: map[string]string{
				labelSessionID: sessionID,
				labelComponent: componentValue,
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

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("pod %q not ready within %s", podName, readyTimeout)
		case <-ticker.C:
			pod, err := m.clientset.CoreV1().Pods(m.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return "", fmt.Errorf("get pod %q: %w", podName, err)
			}
			if isPodReady(pod) {
				return pod.Status.PodIP, nil
			}
		}
	}
}

func (m *SessionManager) createSandboxPod(ctx context.Context, sessionID string) (podIP, podName string, err error) {
	token := computeToken(m.config.HMACKey, sessionID)

	secret := buildAuthSecret(m.config.Namespace, sessionID, token)
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
		return "", "", fmt.Errorf("wait for pod ready: %w", waitErr)
	}
	return ip, created.Name, nil
}

// bestEffortDeleteSecret deletes the per-session auth Secret, logging a
// warning on failure. Shared by SessionManager and WarmPool.
func bestEffortDeleteSecret(ctx context.Context, clientset kubernetes.Interface, namespace, sessionID string, logger *slog.Logger) {
	secretName := secretNamePrefix + sessionID
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
		go m.updateLastActivity(podName)
	}

	return execResp, nil
}

func (m *SessionManager) updateLastActivity(podName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	now := time.Now().UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annotationLastActivity, now)

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

	selector := fmt.Sprintf("%s=%s,%s=%s", labelSessionID, sessionID, labelComponent, componentValue)
	pods, err := m.clientset.CoreV1().Pods(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
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

	secretName := secretNamePrefix + sessionID
	if delErr := m.clientset.CoreV1().Secrets(m.config.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); delErr != nil && !k8serrors.IsNotFound(delErr) {
		errs = append(errs, fmt.Errorf("delete secret %s: %w", secretName, delErr))
	}

	return errors.Join(errs...)
}

// CleanupStale iterates all sandbox pods and cleans up sessions idle longer
// than IdleTimeout. This method is designed to be called externally on a ticker.
func (m *SessionManager) CleanupStale(ctx context.Context) (int, error) {
	selector := fmt.Sprintf("%s=%s", labelComponent, componentValue)
	pods, err := m.clientset.CoreV1().Pods(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return 0, fmt.Errorf("list sandbox pods: %w", err)
	}

	now := time.Now()
	cleaned := 0

	for i := range pods.Items {
		p := &pods.Items[i]
		sessionID := p.Labels[labelSessionID]
		if sessionID == "" {
			continue
		}

		lastActive := p.Annotations[annotationLastActivity]
		if lastActive == "" {
			lastActive = p.Annotations[annotationCreatedAt]
		}
		if lastActive == "" {
			continue
		}

		t, parseErr := time.Parse(time.RFC3339, lastActive)
		if parseErr != nil {
			m.logger.Warn("unparseable timestamp annotation", "pod", p.Name, "value", lastActive)
			continue
		}

		if now.Sub(t) > m.config.IdleTimeout {
			if cleanErr := m.CleanupSession(ctx, sessionID); cleanErr != nil {
				m.logger.Warn("failed to cleanup stale session", "session", sessionID, "error", cleanErr)
				continue
			}
			cleaned++
		}
	}

	return cleaned, nil
}

