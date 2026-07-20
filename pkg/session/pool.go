package session

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codeready-toolchain/cli-mcp-server/pkg/agent"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	agentClient *agent.AgentClient
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
		clientset: clientset,
		config:    config,
		agentClient: agent.NewAgentClient(
			agent.WithPort(config.AgentPort),
			agent.WithTimeout(assignTimeout),
		),
		logger:      logger,
		replenishCh: make(chan struct{}, 1),
	}
}

// SetHTTPClient replaces the agent client's underlying HTTP client (useful for testing).
// The provided client's timeout is preserved.
func (p *WarmPool) SetHTTPClient(c *http.Client) {
	p.agentClient = agent.NewAgentClient(
		agent.WithHTTPClient(c),
		agent.WithPort(p.config.AgentPort),
	)
}

// unassignedSelector returns a label selector matching sandbox pods without a session-id.
func unassignedSelector() string {
	return fmt.Sprintf("%s=%s,!%s", labelComponent, componentValue, labelSessionID)
}

// listUnassignedPods returns all unassigned sandbox pods (including terminal)
// sorted by creation time (oldest first). Callers must filter terminal pods
// as appropriate — ReconcilePool needs to see them for stale cleanup, while
// ClaimPod should skip them.
func (p *WarmPool) listUnassignedPods(ctx context.Context) ([]corev1.Pod, error) {
	podList, err := p.clientset.CoreV1().Pods(p.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: unassignedSelector(),
	})
	if err != nil {
		return nil, fmt.Errorf("list unassigned pods: %w", err)
	}

	pods := podList.Items
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})

	return pods, nil
}

func isTerminalPod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

// buildWarmPodSpec constructs a Pod manifest for a warm pool pod. It uses the
// shared base pod spec with a UUID-suffixed name since warm pods are
// interchangeable. No session-id label or SANDBOX_AUTH_TOKEN env var.
func (p *WarmPool) buildWarmPodSpec() *corev1.Pod {
	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	return buildBasePodSpec(podNamePrefix+suffix, p.config)
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
	var live int

	for i := range pods {
		createdAt := pods[i].Annotations[annotationCreatedAt]
		if createdAt == "" {
			if !isTerminalPod(&pods[i]) {
				live++
			}
			continue
		}
		t, parseErr := time.Parse(time.RFC3339, createdAt)
		if parseErr != nil {
			p.logger.Warn("reconcile: unparseable created-at annotation", "pod", pods[i].Name, "value", createdAt)
			if !isTerminalPod(&pods[i]) {
				live++
			}
			continue
		}
		if now.Sub(t) > staleThreshold {
			p.logger.Info("reconcile: deleting stale unassigned pod", "pod", pods[i].Name, "age", now.Sub(t))
			if delErr := p.clientset.CoreV1().Pods(p.config.Namespace).Delete(ctx, pods[i].Name, metav1.DeleteOptions{}); delErr != nil {
				if k8serrors.IsNotFound(delErr) {
					continue
				}
				p.logger.Warn("reconcile: failed to delete stale pod; skipping refill", "pod", pods[i].Name, "error", delErr)
				return
			}
			continue
		}
		if !isTerminalPod(&pods[i]) {
			live++
		}
	}

	deficit := p.config.WarmPoolSize - live
	if deficit <= 0 {
		return
	}

	p.logger.Info("reconcile: creating warm pods", "current", live, "target", p.config.WarmPoolSize, "creating", deficit)
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
		if isTerminalPod(pod) {
			continue
		}
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
	secret := buildAuthSecret(p.config.Namespace, sessionID, token)
	_, secretErr := p.clientset.CoreV1().Secrets(p.config.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if secretErr != nil {
		p.rollbackLabel(ctx, pod.Name)
		return "", "", fmt.Errorf("create auth secret for claimed pod: %w", secretErr)
	}

	ip := patched.Status.PodIP
	if ip == "" {
		ip, err = p.waitForIP(ctx, patched.Name)
		if err != nil {
			p.rollbackLabel(ctx, pod.Name)
			bestEffortDeleteSecret(ctx, p.clientset, p.config.Namespace, sessionID, p.logger)
			return "", "", fmt.Errorf("wait for pod IP: %w", err)
		}
		patched = patched.DeepCopy()
		patched.Status.PodIP = ip
	}

	if assignErr := p.assignToken(ctx, patched, token); assignErr != nil {
		p.rollbackLabel(ctx, pod.Name)
		bestEffortDeleteSecret(ctx, p.clientset, p.config.Namespace, sessionID, p.logger)
		return "", "", fmt.Errorf("assign token to claimed pod: %w", assignErr)
	}

	return ip, patched.Name, nil
}


// assignToken sends POST /assign to the agent running on the pod to deliver the HMAC token.
func (p *WarmPool) assignToken(ctx context.Context, pod *corev1.Pod, token string) error {
	ip := pod.Status.PodIP
	if ip == "" {
		return fmt.Errorf("pod %s has no IP", pod.Name)
	}

	if err := p.agentClient.Assign(ctx, ip, agent.AssignRequest{Token: token}); err != nil {
		return fmt.Errorf("assign request: %w", err)
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
