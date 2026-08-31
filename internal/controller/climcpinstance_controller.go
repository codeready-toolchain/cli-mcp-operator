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
	"strings"
	"time"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// CliMcpInstanceReconciler reconciles a CliMcpInstance object.
type CliMcpInstanceReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Images      Images
	OnOpenShift bool
}

// Namespaced pods/secrets verbs are a Role in the OperatorGroup target
// namespace (config/rbac/namespaced_role.yaml), not this ClusterRole.
// +kubebuilder:rbac:groups=cli-mcp.redhat.com,resources=climcpinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cli-mcp.redhat.com,resources=climcpinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cli-mcp.redhat.com,resources=climcpinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *CliMcpInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	inst := &climcpv1alpha1.CliMcpInstance{}
	if err := r.Get(ctx, req.NamespacedName, inst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if inst.DeletionTimestamp != nil {
		return r.finalize(ctx, inst)
	}

	if !controllerutil.ContainsFinalizer(inst, finalizerName) {
		controllerutil.AddFinalizer(inst, finalizerName)
		if err := r.Update(ctx, inst); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	hmac, err := r.ensureHMAC(ctx, inst)
	if err != nil {
		return ctrl.Result{}, err
	}

	applyErr := r.applyChildren(ctx, inst, hmac)
	if applyErr != nil {
		logger.Error(applyErr, "apply children")
	}

	idleResult, idleErr := r.idleResult(ctx, inst)
	if idleErr != nil {
		return ctrl.Result{}, idleErr
	}

	if err := r.syncStatus(ctx, inst, hmac, applyErr); err != nil {
		return ctrl.Result{}, err
	}
	if applyErr != nil {
		return ctrl.Result{}, applyErr
	}
	return idleResult, nil
}

func (r *CliMcpInstanceReconciler) finalize(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(inst, finalizerName) {
		return ctrl.Result{}, nil
	}
	logger := logf.FromContext(ctx)

	if err := r.scaleMCPToZero(ctx, inst); err != nil {
		return ctrl.Result{}, err
	}

	var serverPods corev1.PodList
	if err := r.List(ctx, &serverPods, client.InNamespace(inst.Namespace), client.MatchingLabels(serverLabels(inst.Name))); err != nil {
		return ctrl.Result{}, fmt.Errorf("list server pods: %w", err)
	}
	if stillPresent(serverPods.Items) {
		logger.Info("waiting for MCP server pods to terminate")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if err := r.deleteInstanceSandboxes(ctx, inst); err != nil {
		return ctrl.Result{}, err
	}

	var sandboxPods corev1.PodList
	if err := r.List(ctx, &sandboxPods, client.InNamespace(inst.Namespace), client.MatchingLabels(sandboxLabels(inst.Name))); err != nil {
		return ctrl.Result{}, fmt.Errorf("list sandbox pods: %w", err)
	}
	var sessionSecrets corev1.SecretList
	if err := r.List(ctx, &sessionSecrets, client.InNamespace(inst.Namespace), client.MatchingLabels(sandboxLabels(inst.Name))); err != nil {
		return ctrl.Result{}, fmt.Errorf("list session secrets: %w", err)
	}
	if stillPresent(sandboxPods.Items) || secretsPresent(sessionSecrets.Items) {
		logger.Info("waiting for sandbox pods and session secrets to terminate")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(inst, finalizerName)
	if err := r.Update(ctx, inst); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *CliMcpInstanceReconciler) scaleMCPToZero(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	deploy := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: childName(inst.Name), Namespace: inst.Namespace}, deploy)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get MCP Deployment: %w", err)
	}
	if deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == 0 {
		return nil
	}
	zero := int32(0)
	deploy.Spec.Replicas = &zero
	if err := r.Update(ctx, deploy); err != nil {
		return fmt.Errorf("scale MCP Deployment to 0: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) deleteInstanceSandboxes(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	if err := r.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(inst.Namespace), client.MatchingLabels(sandboxLabels(inst.Name))); err != nil {
		return fmt.Errorf("delete sandbox pods: %w", err)
	}
	if err := r.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(inst.Namespace), client.MatchingLabels(sandboxLabels(inst.Name))); err != nil {
		return fmt.Errorf("delete session secrets: %w", err)
	}
	return nil
}

func stillPresent(pods []corev1.Pod) bool {
	return len(pods) > 0
}

func secretsPresent(secrets []corev1.Secret) bool {
	return len(secrets) > 0
}

func (r *CliMcpInstanceReconciler) syncStatus(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret, applyErr error) error {
	orig := inst.DeepCopy()
	inst.Status.WarmPoolDesired = inst.Spec.Sandbox.WarmPoolSize
	inst.Status.WarmPoolReady = 0
	inst.Status.ResolvedSandboxImage = r.resolvedSandboxImage(inst.Spec.Sandbox)

	ready, reason, message := r.readyGate(ctx, inst, hmac, applyErr)
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
		Type:               climcpv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: inst.Generation,
	})

	if equality.Semantic.DeepEqual(orig.Status, inst.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, inst); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) readyGate(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret, applyErr error) (bool, string, string) {
	if applyErr != nil {
		return false, climcpv1alpha1.ReasonReconciling, applyErr.Error()
	}

	if missing, msg := r.missingRequiredSecrets(ctx, inst, hmac); missing {
		return false, climcpv1alpha1.ReasonSecretsNotFound, msg
	}
	if invalid, msg := r.invalidRequiredSecretKeys(ctx, inst, hmac); invalid {
		return false, climcpv1alpha1.ReasonSecretKeysInvalid, msg
	}
	if missing, msg := r.missingChildren(ctx, inst); missing {
		return false, climcpv1alpha1.ReasonChildrenNotReady, msg
	}

	deploy := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: childName(inst.Name), Namespace: inst.Namespace}, deploy)
	if err != nil {
		return false, climcpv1alpha1.ReasonChildrenNotReady, fmt.Sprintf("MCP Deployment: %v", err)
	}
	if !deploymentAvailable(deploy) {
		return false, climcpv1alpha1.ReasonDeploymentUnavailable, "MCP Deployment is not Available"
	}
	return true, climcpv1alpha1.ReasonReady, "instance is ready"
}

func (r *CliMcpInstanceReconciler) missingRequiredSecrets(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret) (bool, string) {
	var missing []string
	kube, err := r.getSecret(ctx, inst.Namespace, kubeconfigSecretName(inst.Name))
	if err != nil {
		return true, err.Error()
	}
	if kube == nil {
		missing = append(missing, kubeconfigSecretName(inst.Name))
	}
	if hmac == nil {
		missing = append(missing, hmacSecretName(inst.Name))
	}
	if !r.OnOpenShift {
		tls, err := r.getSecret(ctx, inst.Namespace, tlsSecretName(inst.Name))
		if err != nil {
			return true, err.Error()
		}
		if tls == nil {
			missing = append(missing, tlsSecretName(inst.Name))
		}
	}
	if len(missing) == 0 {
		return false, ""
	}
	return true, "missing secrets: " + strings.Join(missing, ", ")
}

func (r *CliMcpInstanceReconciler) invalidRequiredSecretKeys(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret) (bool, string) {
	var invalid []string
	kube, err := r.getSecret(ctx, inst.Namespace, kubeconfigSecretName(inst.Name))
	if err != nil {
		return true, err.Error()
	}
	if kube != nil && !secretKeyNonEmpty(kube, kubeconfigDataKey) {
		invalid = append(invalid, kubeconfigSecretName(inst.Name)+"/"+kubeconfigDataKey)
	}
	if hmac != nil && !secretKeyNonEmpty(hmac, hmacSecretKey) {
		invalid = append(invalid, hmacSecretName(inst.Name)+"/"+hmacSecretKey)
	}
	if !r.OnOpenShift {
		tls, err := r.getSecret(ctx, inst.Namespace, tlsSecretName(inst.Name))
		if err != nil {
			return true, err.Error()
		}
		if tls != nil {
			if !secretKeyNonEmpty(tls, tlsCertKey) {
				invalid = append(invalid, tlsSecretName(inst.Name)+"/"+tlsCertKey)
			}
			if !secretKeyNonEmpty(tls, tlsKeyKey) {
				invalid = append(invalid, tlsSecretName(inst.Name)+"/"+tlsKeyKey)
			}
		}
	}
	if len(invalid) == 0 {
		return false, ""
	}
	return true, "empty or missing keys: " + strings.Join(invalid, ", ")
}

func (r *CliMcpInstanceReconciler) missingChildren(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (bool, string) {
	checks := []struct {
		obj  client.Object
		name string
	}{
		{&corev1.ServiceAccount{}, childName(inst.Name)},
		{&corev1.ServiceAccount{}, sandboxSAName(inst.Name)},
		{&rbacv1.Role{}, childName(inst.Name)},
		{&rbacv1.RoleBinding{}, childName(inst.Name)},
		{&corev1.Service{}, childName(inst.Name)},
		{&networkingv1.NetworkPolicy{}, sandboxSAName(inst.Name)},
		{&appsv1.Deployment{}, childName(inst.Name)},
		{&corev1.Secret{}, hmacSecretName(inst.Name)},
	}
	var missing []string
	for _, c := range checks {
		err := r.Get(ctx, types.NamespacedName{Name: c.name, Namespace: inst.Namespace}, c.obj)
		if apierrors.IsNotFound(err) {
			missing = append(missing, c.name)
			continue
		}
		if err != nil {
			return true, err.Error()
		}
	}
	if len(missing) == 0 {
		return false, ""
	}
	return true, "missing children: " + strings.Join(missing, ", ")
}

func (r *CliMcpInstanceReconciler) getSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get secret %s: %w", name, err)
	}
	return secret, nil
}

func deploymentAvailable(d *appsv1.Deployment) bool {
	if d.Status.ObservedGeneration < d.Generation {
		return false
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	if desired < 1 || d.Status.ReadyReplicas < desired {
		return false
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *CliMcpInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&climcpv1alpha1.CliMcpInstance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&corev1.Pod{}, enqueueSandboxPod(), builder.WithPredicates(sandboxPodPredicate{})).
		Watches(&corev1.Secret{}, enqueueSecret()).
		Named("climcpinstance").
		Complete(r)
}
