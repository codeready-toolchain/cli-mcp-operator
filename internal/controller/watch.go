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

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const effectiveKubeconfigIndex = "spec.proxy.effectiveKubeconfig"

func indexEffectiveKubeconfig(obj client.Object) []string {
	inst, ok := obj.(*climcpv1alpha1.CliMcpInstance)
	if !ok {
		return nil
	}
	name, ok := effectiveKubeconfigSecretName(inst)
	if !ok {
		return nil
	}
	return []string{name}
}

func isSandboxPod(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}
	if pod.Labels[session.LabelComponent] != session.ComponentSandbox {
		return false
	}
	return pod.Labels[session.LabelInstance] != ""
}

// sandboxPodPredicate enqueues Create, Delete, session-id assignment, and
// Ready/Failed/backoff updates so pool Ready can converge. It drops
// last-activity annotation patches and routine kubelet status.
type sandboxPodPredicate struct{}

func (sandboxPodPredicate) Create(e event.CreateEvent) bool {
	return isSandboxPod(e.Object)
}

func (sandboxPodPredicate) Delete(e event.DeleteEvent) bool {
	return isSandboxPod(e.Object)
}

func (sandboxPodPredicate) Generic(event.GenericEvent) bool {
	return false
}

func (sandboxPodPredicate) Update(e event.UpdateEvent) bool {
	oldPod, okOld := e.ObjectOld.(*corev1.Pod)
	newPod, okNew := e.ObjectNew.(*corev1.Pod)
	if !okOld || !okNew {
		return false
	}
	if !isSandboxPod(oldPod) && !isSandboxPod(newPod) {
		return false
	}
	_, oldHas := oldPod.Labels[session.LabelSessionID]
	_, newHas := newPod.Labels[session.LabelSessionID]
	if oldHas != newHas {
		return true
	}
	return poolReadinessChanged(oldPod, newPod)
}

func poolReadinessChanged(oldPod, newPod *corev1.Pod) bool {
	if isAssignedSandbox(*oldPod) && isAssignedSandbox(*newPod) {
		return false
	}
	if sandboxPodReady(oldPod) != sandboxPodReady(newPod) {
		return true
	}
	if (oldPod.Status.Phase == corev1.PodFailed) != (newPod.Status.Phase == corev1.PodFailed) {
		return true
	}
	return poolPodUnhealthy(*oldPod) != poolPodUnhealthy(*newPod)
}

func enqueueSandboxPod() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(mapSandboxPod)
}

func mapSandboxPod(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	instance := pod.Labels[session.LabelInstance]
	if instance == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: instance, Namespace: pod.Namespace},
	}}
}

func mapSecret(_ context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	for _, or := range secret.OwnerReferences {
		if or.Kind == "CliMcpInstance" && or.Name != "" {
			return []reconcile.Request{{
				NamespacedName: types.NamespacedName{Name: or.Name, Namespace: secret.Namespace},
			}}
		}
	}
	instance, ok := instanceFromAdminSecret(secret.Name)
	if !ok || instance == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: instance, Namespace: secret.Namespace},
	}}
}

func (r *CliMcpInstanceReconciler) mapSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	reqs := mapSecret(ctx, obj)
	secret, ok := obj.(*corev1.Secret)
	if !ok || r.Client == nil {
		return reqs
	}
	var list climcpv1alpha1.CliMcpInstanceList
	if err := r.List(ctx, &list, client.InNamespace(secret.Namespace), client.MatchingFields{effectiveKubeconfigIndex: secret.Name}); err != nil {
		return reqs
	}
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      list.Items[i].Name,
			Namespace: list.Items[i].Namespace,
		}})
	}
	return dedupeRequests(reqs)
}

func dedupeRequests(reqs []reconcile.Request) []reconcile.Request {
	seen := make(map[types.NamespacedName]struct{}, len(reqs))
	out := make([]reconcile.Request, 0, len(reqs))
	for _, req := range reqs {
		if _, ok := seen[req.NamespacedName]; ok {
			continue
		}
		seen[req.NamespacedName] = struct{}{}
		out = append(out, req)
	}
	return out
}
