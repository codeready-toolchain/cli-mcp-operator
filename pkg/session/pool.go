package session

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/agent"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const assignTimeout = 10 * time.Second

// WarmPool claims unassigned sandbox pods for new sessions. It does not
// replenish the pool; that is the operator's job (Phase 5).
type WarmPool struct {
	clientset   kubernetes.Interface
	config      SandboxConfig
	agentClient *agent.AgentClient
	logger      *slog.Logger
}

// NewWarmPool creates a claim helper. MCP always constructs one so claim
// runs even when no unassigned pods exist (then GetOrCreatePod creates).
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
		logger: logger,
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

func (p *WarmPool) listUnassignedPods(ctx context.Context) ([]corev1.Pod, error) {
	podList, err := p.clientset.CoreV1().Pods(p.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: UnassignedSelector(p.config.InstanceName),
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

// ClaimPod atomically claims an unassigned sandbox pod for the given session.
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
		return "", "", fmt.Errorf("no unassigned pods available")
	}

	for i := range pods {
		pod := &pods[i]
		if isTerminalPod(pod) {
			continue
		}
		ip, name, claimErr := p.tryClaimPod(ctx, pod, sessionID)
		if claimErr == nil {
			return ip, name, nil
		}
		if k8serrors.IsConflict(claimErr) {
			p.logger.Debug("claim: resourceVersion conflict, trying next pod", "pod", pod.Name)
			continue
		}
		p.logger.Warn("claim: failed to claim pod", "pod", pod.Name, "error", claimErr)
	}

	return "", "", fmt.Errorf("all unassigned claim attempts failed")
}

// tryClaimPod attempts to claim a single pod. On failure after label patch, it rolls back.
func (p *WarmPool) tryClaimPod(ctx context.Context, pod *corev1.Pod, sessionID string) (podIP, podName string, err error) {
	patchData := fmt.Sprintf(
		`{"metadata":{"labels":{%q:%q},"resourceVersion":%q}}`,
		LabelSessionID, sessionID, pod.ResourceVersion,
	)
	patched, patchErr := p.clientset.CoreV1().Pods(p.config.Namespace).Patch(
		ctx, pod.Name, types.MergePatchType, []byte(patchData), metav1.PatchOptions{},
	)
	if patchErr != nil {
		return "", "", patchErr
	}

	token := computeToken(p.config.HMACKey, sessionID)
	secret := buildAuthSecret(p.config.Namespace, p.config.InstanceName, sessionID, token)
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

func (p *WarmPool) waitForIP(ctx context.Context, podName string) (string, error) {
	deadline := time.After(readyTimeout)
	ticker := time.NewTicker(readyPollPeriod)
	defer ticker.Stop()

	check := func() (string, bool, error) {
		pod, err := p.clientset.CoreV1().Pods(p.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return "", false, fmt.Errorf("get pod %q: %w", podName, err)
		}
		if pod.Status.PodIP != "" {
			return pod.Status.PodIP, true, nil
		}
		return "", false, nil
	}

	if ip, ok, err := check(); err != nil {
		return "", err
	} else if ok {
		return ip, nil
	}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("pod %q did not get IP within %s", podName, readyTimeout)
		case <-ticker.C:
			ip, ok, err := check()
			if err != nil {
				return "", err
			}
			if ok {
				return ip, nil
			}
		}
	}
}

func (p *WarmPool) rollbackLabel(ctx context.Context, podName string) {
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, LabelSessionID)
	_, err := p.clientset.CoreV1().Pods(p.config.Namespace).Patch(
		ctx, podName, types.MergePatchType, []byte(patch), metav1.PatchOptions{},
	)
	if err != nil {
		p.logger.Warn("rollback: failed to remove session-id label", "pod", podName, "error", err)
	}
}
