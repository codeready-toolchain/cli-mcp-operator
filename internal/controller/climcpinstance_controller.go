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
	"strings"
	"time"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// CliMcpInstanceReconciler reconciles a CliMcpInstance object.
type CliMcpInstanceReconciler struct {
	client.Client
	APIReader   client.Reader
	Scheme      *runtime.Scheme
	Images      Images
	OnOpenShift bool
}

// Namespaced instance-child and pods/secrets verbs are a Role in the
// OperatorGroup target namespace (config/rbac/namespaced_role.yaml), not this
// ClusterRole. bind, unscoped CRB create, and named CRB writes are
// clusterPermissions only.
// +kubebuilder:rbac:groups=cli-mcp.redhat.com,resources=climcpinstances,verbs=get;list;watch;create;update;patch;delete
// Privilege escalation: Role cli-mcp-<name>-client grants climcpinstances/mcp.
// Holding climcpinstances without the subresource is not enough to apply that Role.
// +kubebuilder:rbac:groups=cli-mcp.redhat.com,resources=climcpinstances/mcp,verbs=get;create;delete
// +kubebuilder:rbac:groups=cli-mcp.redhat.com,resources=climcpinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cli-mcp.redhat.com,resources=climcpinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames="system:auth-delegator",verbs=bind
// Collection create has no object name at authorize time, so resourceNames cannot
// scope it. get/update/patch stay pinned to the well-known binding.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,resourceNames=cli-mcp-auth-delegator,verbs=get;update;patch

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

	crbErr := r.applyAuthDelegatorCRB(ctx)
	if crbErr != nil {
		logger.Error(crbErr, "apply auth-delegator ClusterRoleBinding")
	}

	pool, poolErr := r.reconcilePool(ctx, inst, applyErr == nil)
	if poolErr != nil {
		logger.Error(poolErr, "reconcile warm pool")
	}

	idleResult, idleErr := r.idleResult(ctx, inst)
	if idleErr != nil {
		return ctrl.Result{}, idleErr
	}

	reconcileErr := applyErr
	if reconcileErr == nil {
		reconcileErr = crbErr
	}
	if reconcileErr == nil {
		reconcileErr = poolErr
	}
	orig := inst.DeepCopy()
	if err := r.syncStatus(ctx, inst, hmac, applyErr, crbErr, pool); err != nil {
		return ctrl.Result{}, err
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	return ctrl.Result{RequeueAfter: scheduledRequeueAfter(idleResult.RequeueAfter, poolRequeueAfter(orig, pool, time.Now().UTC()))}, nil
}

func (r *CliMcpInstanceReconciler) finalize(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(inst, finalizerName) {
		return ctrl.Result{}, nil
	}
	logger := logf.FromContext(ctx)

	if err := r.applyAuthDelegatorCRB(ctx); err != nil {
		return ctrl.Result{}, err
	}

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

func (r *CliMcpInstanceReconciler) syncStatus(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret, applyErr, crbErr error, pool poolSnapshot) error {
	orig := inst.DeepCopy()
	inst.Status.WarmPoolDesired = pool.desired
	inst.Status.WarmPoolReady = pool.ready
	inst.Status.ResolvedSandboxImage = r.resolvedSandboxImage(inst.Spec.Sandbox)
	inst.Status.ClientServiceAccount = clientSAName(inst.Name)

	poolFull := pool.desired == 0 || pool.ready >= pool.desired
	warmStatus := metav1.ConditionFalse
	warmReason := climcpv1alpha1.ReasonWarmPoolNotReady
	warmMsg := fmt.Sprintf("warm pool %d/%d", pool.ready, pool.desired)
	if pool.unhealthy {
		warmReason = climcpv1alpha1.ReasonWarmPoolUnhealthy
		warmMsg = pool.unhealthyMsg
	} else if poolFull {
		warmStatus = metav1.ConditionTrue
		warmReason = climcpv1alpha1.ReasonReady
		warmMsg = "warm pool is ready"
	}
	meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
		Type:               climcpv1alpha1.ConditionWarmPoolReady,
		Status:             warmStatus,
		Reason:             warmReason,
		Message:            warmMsg,
		ObservedGeneration: inst.Generation,
	})

	ready, reason, message := r.readyGate(ctx, orig, inst, hmac, applyErr, crbErr, pool)
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

func (r *CliMcpInstanceReconciler) readyGate(ctx context.Context, orig, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret, applyErr, crbErr error, pool poolSnapshot) (bool, string, string) {
	if crbErr != nil {
		return false, climcpv1alpha1.ReasonChildrenNotReady, crbErr.Error()
	}
	if applyErr != nil {
		return false, climcpv1alpha1.ReasonReconciling, applyErr.Error()
	}

	if missing, msg := r.missingRequiredSecrets(ctx, inst, hmac); missing {
		return false, climcpv1alpha1.ReasonSecretsNotFound, msg
	}
	if invalid, msg := r.invalidRequiredSecretKeys(ctx, inst, hmac); invalid {
		return false, climcpv1alpha1.ReasonSecretKeysInvalid, msg
	}
	if invalid, msg := r.kubeconfigInvalid(ctx, inst); invalid {
		return false, climcpv1alpha1.ReasonKubeconfigInvalid, msg
	}
	if missing, msg := r.missingChildren(ctx, inst); missing {
		return false, climcpv1alpha1.ReasonChildrenNotReady, msg
	}
	if missing, msg := r.sandboxEgressNotReady(ctx, inst); missing {
		return false, climcpv1alpha1.ReasonChildrenNotReady, msg
	}
	if missing, msg := r.authDelegatorNotReady(ctx, inst); missing {
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
	proxyDeploy := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: proxyName(inst.Name), Namespace: inst.Namespace}, proxyDeploy)
	if err != nil {
		return false, climcpv1alpha1.ReasonChildrenNotReady, fmt.Sprintf("proxy Deployment: %v", err)
	}
	if !deploymentAvailable(proxyDeploy) {
		return false, climcpv1alpha1.ReasonDeploymentUnavailable, "proxy Deployment is not Available"
	}
	return poolReadyGate(orig, pool, time.Now().UTC())
}

func (r *CliMcpInstanceReconciler) missingRequiredSecrets(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret) (bool, string) {
	var missing []string
	if name, ok := effectiveKubeconfigSecretName(inst); ok {
		kube, err := r.getSecret(ctx, inst.Namespace, name)
		if err != nil {
			return true, err.Error()
		}
		if kube == nil {
			missing = append(missing, name)
		}
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
	if name, ok := effectiveKubeconfigSecretName(inst); ok {
		kube, err := r.getSecret(ctx, inst.Namespace, name)
		if err != nil {
			return true, err.Error()
		}
		if kube != nil && !secretKeyNonEmpty(kube, kubeconfigDataKey) {
			invalid = append(invalid, name+"/"+kubeconfigDataKey)
		}
	}
	ca, err := r.getSecret(ctx, inst.Namespace, proxyCASecretName(inst.Name))
	if err != nil {
		return true, err.Error()
	}
	if ca != nil {
		if !secretKeyNonEmpty(ca, caCertKey) {
			invalid = append(invalid, proxyCASecretName(inst.Name)+"/"+caCertKey)
		}
		if !secretKeyNonEmpty(ca, caKeyKey) {
			invalid = append(invalid, proxyCASecretName(inst.Name)+"/"+caKeyKey)
		}
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

func (r *CliMcpInstanceReconciler) kubeconfigInvalid(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (bool, string) {
	name, ok := effectiveKubeconfigSecretName(inst)
	if !ok {
		return false, ""
	}
	kube, err := r.getSecret(ctx, inst.Namespace, name)
	if err != nil {
		return true, err.Error()
	}
	if kube == nil || !secretKeyNonEmpty(kube, kubeconfigDataKey) {
		return false, ""
	}
	if _, err := kubeconfig.Validate(kube.Data[kubeconfigDataKey]); err != nil {
		return true, err.Error()
	}
	return false, ""
}

func (r *CliMcpInstanceReconciler) sandboxEgressNotReady(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (bool, string) {
	np := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: sandboxSAName(inst.Name), Namespace: inst.Namespace}, np)
	if apierrors.IsNotFound(err) {
		return true, "sandbox NetworkPolicy is missing"
	}
	if err != nil {
		return true, err.Error()
	}
	if !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		return true, "sandbox NetworkPolicy does not set Egress"
	}
	return false, ""
}

func (r *CliMcpInstanceReconciler) missingChildren(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (bool, string) {
	checks := []struct {
		obj  client.Object
		name string
	}{
		{&corev1.ServiceAccount{}, childName(inst.Name)},
		{&corev1.ServiceAccount{}, sandboxSAName(inst.Name)},
		{&corev1.ServiceAccount{}, clientSAName(inst.Name)},
		{&rbacv1.Role{}, childName(inst.Name)},
		{&rbacv1.Role{}, clientSAName(inst.Name)},
		{&rbacv1.RoleBinding{}, childName(inst.Name)},
		{&rbacv1.RoleBinding{}, clientSAName(inst.Name)},
		{&corev1.Service{}, childName(inst.Name)},
		{&networkingv1.NetworkPolicy{}, sandboxSAName(inst.Name)},
		{&networkingv1.NetworkPolicy{}, proxyName(inst.Name)},
		{&appsv1.Deployment{}, childName(inst.Name)},
		{&appsv1.Deployment{}, proxyName(inst.Name)},
		{&corev1.Service{}, proxyName(inst.Name)},
		{&corev1.ServiceAccount{}, proxyName(inst.Name)},
		{&corev1.Secret{}, hmacSecretName(inst.Name)},
		{&corev1.Secret{}, proxyCASecretName(inst.Name)},
		{&corev1.ConfigMap{}, krpConfigMapName(inst.Name)},
		{&corev1.ConfigMap{}, proxyName(inst.Name)},
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
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &climcpv1alpha1.CliMcpInstance{}, effectiveKubeconfigIndex, indexEffectiveKubeconfig); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&climcpv1alpha1.CliMcpInstance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Pod{}, enqueueSandboxPod(), builder.WithPredicates(sandboxPodPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecret)).
		Named("climcpinstance").
		Complete(r)
}
