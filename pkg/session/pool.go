package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/agent"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultReconcileInterval = 30 * time.Second
	assignTimeout            = 10 * time.Second
)

// WarmPool manages a pre-created set of unassigned sandbox pods that can be
// claimed instantly by new sessions, eliminating cold-start latency.
type WarmPool struct {
	clientset   kubernetes.Interface
	config      SandboxConfig
	httpClient  *http.Client
	logger      *slog.Logger
	replenishCh chan struct{}
}

// NewWarmPool creates a WarmPool. The pool does not start reconciliation
// until StartReconciler is called.
func NewWarmPool(clientset kubernetes.Interface, config SandboxConfig, logger *slog.Logger) *WarmPool {
	if logger == nil {
		logger = slog.Default()
	}
	return &WarmPool{
		clientset:   clientset,
		config:      config,
		httpClient:  &http.Client{Timeout: assignTimeout},
		logger:      logger,
		replenishCh: make(chan struct{}, 1),
	}
}

// SetHTTPClient replaces the default HTTP client (useful for testing).
func (p *WarmPool) SetHTTPClient(c *http.Client) {
	p.httpClient = c
}

// unassignedSelector returns a label selector matching sandbox pods without a session-id.
func unassignedSelector() string {
	return fmt.Sprintf("%s=%s,!%s", labelComponent, componentValue, labelSessionID)
}

// listUnassignedPods returns non-terminal unassigned sandbox pods sorted by creation time (oldest first).
func (p *WarmPool) listUnassignedPods(ctx context.Context) ([]corev1.Pod, error) {
	podList, err := p.clientset.CoreV1().Pods(p.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: unassignedSelector(),
	})
	if err != nil {
		return nil, fmt.Errorf("list unassigned pods: %w", err)
	}

	var pods []corev1.Pod
	for i := range podList.Items {
		phase := podList.Items[i].Status.Phase
		if phase == corev1.PodFailed || phase == corev1.PodSucceeded {
			continue
		}
		pods = append(pods, podList.Items[i])
	}

	sort.Slice(pods, func(i, j int) bool {
		return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})

	return pods, nil
}

// buildWarmPodSpec constructs a Pod manifest for a warm pool pod. It matches
// the session manager's buildPodSpec but omits session-specific fields
// (no session-id label, no SANDBOX_AUTH_TOKEN env var) and uses a UUID-suffixed
// name since warm pods are interchangeable.
func (p *WarmPool) buildWarmPodSpec() *corev1.Pod {
	now := time.Now().UTC().Format(time.RFC3339)
	runAsNonRoot := true
	var runAsUser int64 = 1001
	var runAsGroup int64 = 1001
	allowPrivEsc := false

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podNamePrefix + suffix,
			Namespace: p.config.Namespace,
			Labels: map[string]string{
				labelComponent: componentValue,
			},
			Annotations: map[string]string{
				annotationCreatedAt: now,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: p.config.ServiceAccountName,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				RunAsUser:    &runAsUser,
				RunAsGroup:   &runAsGroup,
			},
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: p.config.Image,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(p.config.CPURequest),
							corev1.ResourceMemory: resource.MustParse(p.config.MemoryRequest),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(p.config.CPULimit),
							corev1.ResourceMemory: resource.MustParse(p.config.MemoryLimit),
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromInt32(int32(p.config.AgentPort)),
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
							SecretName: p.config.KubeconfigSecret,
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

// ReconcilePool ensures the number of unassigned warm pods matches WarmPoolSize.
// It first deletes stale unassigned pods (age > 2× IdleTimeout), then creates
// new pods to fill the deficit.
func (p *WarmPool) ReconcilePool(ctx context.Context) {
	pods, err := p.listUnassignedPods(ctx)
	if err != nil {
		p.logger.Error("reconcile: failed to list unassigned pods", "error", err)
		return
	}

	staleThreshold := 2 * p.config.IdleTimeout
	now := time.Now()
	var live []corev1.Pod

	for i := range pods {
		createdAt := pods[i].Annotations[annotationCreatedAt]
		if createdAt == "" {
			live = append(live, pods[i])
			continue
		}
		t, parseErr := time.Parse(time.RFC3339, createdAt)
		if parseErr != nil {
			p.logger.Warn("reconcile: unparseable created-at annotation", "pod", pods[i].Name, "value", createdAt)
			live = append(live, pods[i])
			continue
		}
		if now.Sub(t) > staleThreshold {
			p.logger.Info("reconcile: deleting stale unassigned pod", "pod", pods[i].Name, "age", now.Sub(t))
			if delErr := p.clientset.CoreV1().Pods(p.config.Namespace).Delete(ctx, pods[i].Name, metav1.DeleteOptions{}); delErr != nil && !k8serrors.IsNotFound(delErr) {
				p.logger.Warn("reconcile: failed to delete stale pod", "pod", pods[i].Name, "error", delErr)
			}
			continue
		}
		live = append(live, pods[i])
	}

	deficit := p.config.WarmPoolSize - len(live)
	if deficit <= 0 {
		return
	}

	p.logger.Info("reconcile: creating warm pods", "current", len(live), "target", p.config.WarmPoolSize, "creating", deficit)
	for range deficit {
		pod := p.buildWarmPodSpec()
		_, createErr := p.clientset.CoreV1().Pods(p.config.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		if createErr != nil {
			p.logger.Error("reconcile: failed to create warm pod", "error", createErr)
		}
	}
}

// ClaimPod atomically claims an unassigned warm pod for the given session.
// It patches the oldest unassigned pod with the session-id label using
// resourceVersion for optimistic locking, creates the auth Secret, and
// delivers the token via POST /assign to the agent.
//
// On any failure after the label patch, it rolls back all changes.
// Returns the pod IP and name, or an error if no pod could be claimed.
func (p *WarmPool) ClaimPod(ctx context.Context, sessionID string) (podIP, podName string, err error) {
	pods, err := p.listUnassignedPods(ctx)
	if err != nil {
		return "", "", fmt.Errorf("claim: %w", err)
	}
	if len(pods) == 0 {
		return "", "", fmt.Errorf("warm pool exhausted: no unassigned pods available")
	}

	for i := range pods {
		pod := &pods[i]
		ip, name, claimErr := p.tryClaimPod(ctx, pod, sessionID)
		if claimErr == nil {
			p.TriggerReplenish()
			return ip, name, nil
		}
		if k8serrors.IsConflict(claimErr) {
			p.logger.Debug("claim: resourceVersion conflict, trying next pod", "pod", pod.Name)
			continue
		}
		p.logger.Warn("claim: failed to claim pod", "pod", pod.Name, "error", claimErr)
	}

	return "", "", fmt.Errorf("warm pool exhausted: all claim attempts failed")
}

// tryClaimPod attempts to claim a single pod. On failure after label patch, it rolls back.
func (p *WarmPool) tryClaimPod(ctx context.Context, pod *corev1.Pod, sessionID string) (podIP, podName string, err error) {
	patchData := fmt.Sprintf(
		`{"metadata":{"labels":{%q:%q},"resourceVersion":%q}}`,
		labelSessionID, sessionID, pod.ResourceVersion,
	)
	patched, patchErr := p.clientset.CoreV1().Pods(p.config.Namespace).Patch(
		ctx, pod.Name, types.MergePatchType, []byte(patchData), metav1.PatchOptions{},
	)
	if patchErr != nil {
		return "", "", patchErr
	}

	token := computeToken(p.config.HMACKey, sessionID)
	secret := p.buildAuthSecret(sessionID, token)
	_, secretErr := p.clientset.CoreV1().Secrets(p.config.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if secretErr != nil && !k8serrors.IsAlreadyExists(secretErr) {
		p.rollbackLabel(ctx, pod.Name)
		return "", "", fmt.Errorf("create auth secret for claimed pod: %w", secretErr)
	}

	if assignErr := p.assignToken(ctx, patched, token); assignErr != nil {
		p.rollbackLabel(ctx, pod.Name)
		p.bestEffortDeleteSecret(ctx, sessionID)
		return "", "", fmt.Errorf("assign token to claimed pod: %w", assignErr)
	}

	ip := patched.Status.PodIP
	if ip == "" {
		ip, err = p.waitForIP(ctx, patched.Name)
		if err != nil {
			p.rollbackLabel(ctx, pod.Name)
			p.bestEffortDeleteSecret(ctx, sessionID)
			return "", "", fmt.Errorf("wait for pod IP: %w", err)
		}
	}

	return ip, patched.Name, nil
}

func (p *WarmPool) buildAuthSecret(sessionID, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretNamePrefix + sessionID,
			Namespace: p.config.Namespace,
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

// assignToken sends POST /assign to the agent running on the pod to deliver the HMAC token.
func (p *WarmPool) assignToken(ctx context.Context, pod *corev1.Pod, token string) error {
	ip := pod.Status.PodIP
	if ip == "" {
		return fmt.Errorf("pod %s has no IP", pod.Name)
	}

	reqBody := agent.AssignRequest{Token: token}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal assign request: %w", err)
	}

	host := net.JoinHostPort(ip, fmt.Sprintf("%d", p.config.AgentPort))
	url := fmt.Sprintf("http://%s/assign", host)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create assign request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("assign request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("assign returned status %d", resp.StatusCode)
	}
	return nil
}

// waitForIP polls for the pod to have an IP assigned (for pods that are ready
// but the IP wasn't yet reflected when we read the pod).
func (p *WarmPool) waitForIP(ctx context.Context, podName string) (string, error) {
	deadline := time.After(readyTimeout)
	ticker := time.NewTicker(readyPollPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("pod %q did not get IP within %s", podName, readyTimeout)
		case <-ticker.C:
			pod, err := p.clientset.CoreV1().Pods(p.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return "", fmt.Errorf("get pod %q: %w", podName, err)
			}
			if pod.Status.PodIP != "" {
				return pod.Status.PodIP, nil
			}
		}
	}
}

// rollbackLabel removes the session-id label from a pod (best-effort).
func (p *WarmPool) rollbackLabel(ctx context.Context, podName string) {
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, labelSessionID)
	_, err := p.clientset.CoreV1().Pods(p.config.Namespace).Patch(
		ctx, podName, types.MergePatchType, []byte(patch), metav1.PatchOptions{},
	)
	if err != nil {
		p.logger.Warn("rollback: failed to remove session-id label", "pod", podName, "error", err)
	}
}

func (p *WarmPool) bestEffortDeleteSecret(ctx context.Context, sessionID string) {
	secretName := secretNamePrefix + sessionID
	err := p.clientset.CoreV1().Secrets(p.config.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		p.logger.Warn("rollback: failed to delete auth secret", "secret", secretName, "error", err)
	}
}

// TriggerReplenish sends a non-blocking signal to the reconciler to run
// immediately. Multiple rapid calls coalesce into a single reconciliation.
func (p *WarmPool) TriggerReplenish() {
	select {
	case p.replenishCh <- struct{}{}:
	default:
	}
}

// StartReconciler runs a background goroutine that calls ReconcilePool on a
// periodic ticker and whenever TriggerReplenish signals. It exits when ctx
// is cancelled.
func (p *WarmPool) StartReconciler(ctx context.Context) {
	interval := p.config.ReconcileInterval
	if interval <= 0 {
		interval = defaultReconcileInterval
	}

	ticker := time.NewTicker(interval)

	go func() {
		defer ticker.Stop()

		p.ReconcilePool(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.ReconcilePool(ctx)
			case <-p.replenishCh:
				p.ReconcilePool(ctx)
			}
		}
	}()
}
