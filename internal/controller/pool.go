/*
Copyright 2026 CodeReady Toolchain.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	poolPodNamePrefix       = "cli-mcp-sandbox-"
	poolReplenishDeadline   = 5 * time.Minute
	waitingImagePullBackOff = "ImagePullBackOff"
	waitingCrashLoopBackOff = "CrashLoopBackOff"
	waitingErrImagePull     = "ErrImagePull"
)

type poolSnapshot struct {
	desired      int32
	ready        int32
	unhealthy    bool
	unhealthyMsg string
}

func isAssignedSandbox(pod corev1.Pod) bool {
	_, assigned := pod.Labels[session.LabelSessionID]
	return assigned
}

func isUnassignedSandbox(pod corev1.Pod) bool {
	return !isAssignedSandbox(pod)
}

func sandboxPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func poolPodUnhealthy(pod corev1.Pod) bool {
	if isAssignedSandbox(pod) || pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Status.Phase == corev1.PodFailed {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		switch cs.State.Waiting.Reason {
		case waitingImagePullBackOff, waitingCrashLoopBackOff, waitingErrImagePull:
			return true
		}
	}
	return false
}

func observePool(pods []corev1.Pod, desired int32) poolSnapshot {
	snap := poolSnapshot{desired: desired}
	for i := range pods {
		pod := pods[i]
		if !isUnassignedSandbox(pod) {
			continue
		}
		if poolPodUnhealthy(pod) {
			snap.unhealthy = true
			snap.unhealthyMsg = fmt.Sprintf("warm pool pod %s is Failed or in backoff", pod.Name)
		}
		if sandboxPodReady(&pod) {
			snap.ready++
		}
	}
	return snap
}

func (r *CliMcpInstanceReconciler) reconcilePool(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, mutate bool) (poolSnapshot, error) {
	desired := inst.Spec.Sandbox.WarmPoolSize
	fail := poolSnapshot{desired: desired}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(inst.Namespace), client.MatchingLabels(sandboxLabels(inst.Name))); err != nil {
		return fail, fmt.Errorf("list sandbox pods: %w", err)
	}

	if !mutate {
		return observePool(pods.Items, desired), nil
	}

	cfg := r.poolSandboxConfig(inst)
	hash, err := overlayHash(cfg)
	if err != nil {
		return observePool(pods.Items, desired), err
	}
	if desired > 0 && cfg.Image == "" {
		return observePool(pods.Items, desired), fmt.Errorf("sandbox image is empty: set spec.sandbox.image or %s", envRelatedImageSandbox)
	}

	// Failed/backoff unassigned pods do not occupy pool slots; re-get skips claim.
	// Terminating unassigned pods still occupy slots so we do not overshoot.
	pre := observePool(pods.Items, desired)

	var keep []corev1.Pod
	for i := range pods.Items {
		pod := pods.Items[i]
		if !isUnassignedSandbox(pod) {
			continue
		}
		if pod.DeletionTimestamp != nil {
			keep = append(keep, pod)
			continue
		}
		stale := pod.Annotations[sandboxOverlayAnnotation] != hash
		if stale || poolPodUnhealthy(pod) {
			if err := r.deleteUnassignedIfStillUnassigned(ctx, &pod); err != nil {
				return r.observeAfterMutate(ctx, inst, desired, pre, err)
			}
			continue
		}
		keep = append(keep, pod)
	}

	slices.SortFunc(keep, func(a, b corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})

	surplus := max(len(keep)-int(desired), 0)
	for i := range surplus {
		if err := r.deleteUnassignedIfStillUnassigned(ctx, &keep[i]); err != nil {
			return r.observeAfterMutate(ctx, inst, desired, pre, err)
		}
	}
	keep = keep[surplus:]

	deficit := int(desired) - len(keep)
	for range deficit {
		if err := r.createPoolPod(ctx, inst, cfg, hash); err != nil {
			return r.observeAfterMutate(ctx, inst, desired, pre, err)
		}
	}

	return r.observeAfterMutate(ctx, inst, desired, pre, nil)
}

// observeAfterMutate re-lists sandbox pods so status matches the cluster after
// creates/deletes. mutateErr is returned as-is; a re-list failure wraps it.
func (r *CliMcpInstanceReconciler) observeAfterMutate(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, desired int32, pre poolSnapshot, mutateErr error) (poolSnapshot, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(inst.Namespace), client.MatchingLabels(sandboxLabels(inst.Name))); err != nil {
		if mutateErr != nil {
			return poolSnapshot{desired: desired}, fmt.Errorf("%w (re-list: %w)", mutateErr, err)
		}
		return poolSnapshot{desired: desired}, fmt.Errorf("list sandbox pods after pool adjust: %w", err)
	}
	post := observePool(pods.Items, desired)
	if pre.unhealthy {
		post.unhealthy = true
		if post.unhealthyMsg == "" {
			post.unhealthyMsg = pre.unhealthyMsg
		}
	}
	return post, mutateErr
}

func (r *CliMcpInstanceReconciler) deleteUnassignedIfStillUnassigned(ctx context.Context, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)
	fresh := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, fresh)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("re-get pool pod %s: %w", pod.Name, err)
	}
	if isAssignedSandbox(*fresh) {
		logger.Info("skipping pool delete; session-id appeared", "pod", fresh.Name)
		return nil
	}
	logger.Info("deleting unassigned pool pod", "pod", fresh.Name)
	uid := fresh.UID
	rv := fresh.ResourceVersion
	deleteOpts := &client.DeleteOptions{
		Preconditions: &metav1.Preconditions{
			UID:             &uid,
			ResourceVersion: &rv,
		},
	}
	if err := r.Delete(ctx, fresh, deleteOpts); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			logger.Info("pool delete skipped; pod changed concurrently", "pod", fresh.Name)
			return nil
		}
		return fmt.Errorf("delete pool pod %s: %w", fresh.Name, err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) createPoolPod(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, cfg session.SandboxConfig, hash string) error {
	pod := session.BuildBasePodSpec(poolPodNamePrefix+uuid.NewString(), cfg)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[sandboxOverlayAnnotation] = hash
	if err := controllerutil.SetControllerReference(inst, pod, r.Scheme); err != nil {
		return fmt.Errorf("pool pod ownerRef: %w", err)
	}
	if err := r.Create(ctx, pod); err != nil {
		return fmt.Errorf("create pool pod: %w", err)
	}
	return nil
}

func poolReadyGate(orig *climcpv1alpha1.CliMcpInstance, pool poolSnapshot, now time.Time) (bool, string, string) {
	if pool.desired == 0 {
		return true, climcpv1alpha1.ReasonReady, "instance is ready"
	}
	if pool.unhealthy {
		return false, climcpv1alpha1.ReasonWarmPoolUnhealthy, pool.unhealthyMsg
	}
	if pool.ready >= pool.desired {
		return true, climcpv1alpha1.ReasonReady, "instance is ready"
	}

	previouslyReady := meta.IsStatusConditionTrue(orig.Status.Conditions, climcpv1alpha1.ConditionReady)
	increased := orig.Status.WarmPoolDesired < pool.desired
	if increased || !previouslyReady {
		return false, climcpv1alpha1.ReasonWarmPoolNotReady, fmt.Sprintf("waiting for warm pool %d/%d", pool.ready, pool.desired)
	}
	if shortfallPastDeadline(orig, now) {
		return false, climcpv1alpha1.ReasonWarmPoolNotReady, "warm pool shortfall exceeded replenish deadline"
	}
	return true, climcpv1alpha1.ReasonReady, "instance is ready"
}

func shortfallPastDeadline(orig *climcpv1alpha1.CliMcpInstance, now time.Time) bool {
	c := meta.FindStatusCondition(orig.Status.Conditions, climcpv1alpha1.ConditionWarmPoolReady)
	if c == nil || c.Status != metav1.ConditionFalse {
		return false
	}
	return !c.LastTransitionTime.Time.Add(poolReplenishDeadline).After(now)
}

func poolRequeueAfter(orig *climcpv1alpha1.CliMcpInstance, pool poolSnapshot, now time.Time) time.Duration {
	if pool.desired == 0 || pool.unhealthy || pool.ready >= pool.desired {
		return 0
	}
	if !meta.IsStatusConditionTrue(orig.Status.Conditions, climcpv1alpha1.ConditionReady) {
		return 0
	}
	c := meta.FindStatusCondition(orig.Status.Conditions, climcpv1alpha1.ConditionWarmPoolReady)
	start := now
	if c != nil && c.Status == metav1.ConditionFalse && !c.LastTransitionTime.Time.IsZero() {
		start = c.LastTransitionTime.Time
	}
	remaining := start.Add(poolReplenishDeadline).Sub(now)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func mergeRequeueAfter(a, b time.Duration) time.Duration {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	default:
		return min(a, b)
	}
}
