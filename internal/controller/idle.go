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
	"time"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const minIdleRequeue = time.Second

func activityTime(pod corev1.Pod) (time.Time, bool) {
	raw := pod.Annotations[session.AnnotationLastActivity]
	if raw == "" {
		raw = pod.Annotations[session.AnnotationCreatedAt]
	}
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func idleExpired(pod corev1.Pod, now time.Time, timeout time.Duration) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	if _, assigned := pod.Labels[session.LabelSessionID]; !assigned {
		return false
	}
	at, ok := activityTime(pod)
	if !ok {
		return false
	}
	return !at.Add(timeout).After(now)
}

func soonestIdleRequeue(pods []corev1.Pod, now time.Time, timeout time.Duration) time.Duration {
	var next time.Duration
	for i := range pods {
		pod := pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if _, assigned := pod.Labels[session.LabelSessionID]; !assigned {
			continue
		}
		at, ok := activityTime(pod)
		if !ok {
			continue
		}
		delay := at.Add(timeout).Sub(now)
		if delay <= 0 {
			continue
		}
		if next == 0 || delay < next {
			next = delay
		}
	}
	if next == 0 {
		return 0
	}
	return max(next, minIdleRequeue)
}

func (r *CliMcpInstanceReconciler) gcIdleAssigned(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, now time.Time) (time.Duration, error) {
	timeout := idleTimeoutOrDefault(inst.Spec.Sandbox.IdleTimeout.Duration)
	sel, err := labels.Parse(session.AssignedAnySelector(inst.Name))
	if err != nil {
		return 0, fmt.Errorf("parse assigned selector: %w", err)
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(inst.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return 0, fmt.Errorf("list assigned sandbox pods: %w", err)
	}

	logger := log.FromContext(ctx)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !idleExpired(*pod, now, timeout) {
			continue
		}
		sessionID := pod.Labels[session.LabelSessionID]
		logger.Info("idle-GC assigned sandbox", "pod", pod.Name, "session", sessionID)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("delete idle pod %s: %w", pod.Name, err)
		}
		if sessionID == "" {
			continue
		}
		secret := &corev1.Secret{}
		secret.Name = session.AuthSecretName(sessionID)
		secret.Namespace = inst.Namespace
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("delete idle session secret %s: %w", secret.Name, err)
		}
	}

	return soonestIdleRequeue(pods.Items, now, timeout), nil
}

func (r *CliMcpInstanceReconciler) idleResult(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (ctrl.Result, error) {
	requeueAfter, err := r.gcIdleAssigned(ctx, inst, time.Now().UTC())
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{}, nil
}
