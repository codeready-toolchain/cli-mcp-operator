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
	"encoding/json"
	"fmt"
	"maps"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *CliMcpInstanceReconciler) applyChildren(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret) error {
	if err := r.applySandboxSA(ctx, inst); err != nil {
		return err
	}
	if err := r.applyMCPSA(ctx, inst); err != nil {
		return err
	}
	if err := r.applyMCPRole(ctx, inst); err != nil {
		return err
	}
	if err := r.applyMCPRoleBinding(ctx, inst); err != nil {
		return err
	}
	if err := r.applyAuthDelegator(ctx, inst); err != nil {
		return err
	}
	if err := r.deleteLegacyMCPSA(ctx, inst); err != nil {
		return err
	}
	if err := r.applyService(ctx, inst); err != nil {
		return err
	}
	if err := r.applySandboxIngressNP(ctx, inst); err != nil {
		return err
	}
	if err := r.deleteLegacySandbox(ctx, inst); err != nil {
		return err
	}
	if err := r.applyDeployment(ctx, inst, hmac); err != nil {
		return err
	}
	return r.deleteLegacyServerWorkload(ctx, inst)
}

func (r *CliMcpInstanceReconciler) applySandboxSA(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      sandboxSAName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = instanceLabels(inst.Name)
		automount := false
		sa.AutomountServiceAccountToken = &automount
		return controllerutil.SetControllerReference(inst, sa, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply sandbox SA: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) applyMCPSA(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      mcpServerSAName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = instanceLabels(inst.Name)
		return controllerutil.SetControllerReference(inst, sa, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply MCP SA: %w", err)
	}
	return nil
}

func mcpRoleRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"create", "get", "list", "watch", "update", "patch", "delete"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"create", "delete"},
		},
	}
}

func (r *CliMcpInstanceReconciler) applyMCPRole(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name:      childName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = instanceLabels(inst.Name)
		role.Rules = mcpRoleRules()
		return controllerutil.SetControllerReference(inst, role, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply MCP Role: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) applyMCPRoleBinding(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      childName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Labels = instanceLabels(inst.Name)
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     childName(inst.Name),
		}
		rb.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      mcpServerSAName(inst.Name),
			Namespace: inst.Namespace,
		}}
		return controllerutil.SetControllerReference(inst, rb, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply MCP RoleBinding: %w", err)
	}
	return nil
}

// applyAuthDelegator binds the MCP server SA to system:auth-delegator so
// kube-rbac-proxy can TokenReview / SubjectAccessReview. Cluster-scoped: no
// ownerRef from the namespaced CR; the instance finalizer deletes this object.
func (r *CliMcpInstanceReconciler) applyAuthDelegator(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: authDelegatorCRBName(inst.Namespace, inst.Name),
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		crb.Labels = authDelegatorLabels(inst.Namespace, inst.Name)
		crb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     authDelegatorClusterRole,
		}
		crb.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      mcpServerSAName(inst.Name),
			Namespace: inst.Namespace,
		}}
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply auth-delegator ClusterRoleBinding: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) deleteAuthDelegator(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: authDelegatorCRBName(inst.Namespace, inst.Name),
	}}
	if err := r.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete auth-delegator ClusterRoleBinding: %w", err)
	}
	return nil
}

// deleteLegacyMCPSA removes older MCP pod SAs: cli-mcp-<instance> (Deployment
// name) and cli-mcp-<instance>-server. The current identity is
// cli-mcp-server-<instance>.
func (r *CliMcpInstanceReconciler) deleteLegacyMCPSA(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	want := mcpServerSAName(inst.Name)
	for _, name := range []string{legacyServerChildName(inst.Name), legacyServerChildName(inst.Name) + "-server"} {
		if name == want {
			continue
		}
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: inst.Namespace,
		}}
		if err := r.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete legacy MCP SA %s: %w", name, err)
		}
	}
	return nil
}

func (r *CliMcpInstanceReconciler) deleteLegacySandbox(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	legacy := childNamePrefix + inst.Name + "-sandbox"
	if legacy == sandboxSAName(inst.Name) {
		return nil
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      legacy,
		Namespace: inst.Namespace,
	}}
	if err := r.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy sandbox SA: %w", err)
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name:      legacy,
		Namespace: inst.Namespace,
	}}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy sandbox NetworkPolicy: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) deleteLegacyServerWorkload(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	legacy := legacyServerChildName(inst.Name)
	if legacy == childName(inst.Name) {
		return nil
	}
	ns := inst.Namespace
	if err := r.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: legacy, Namespace: ns}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy Deployment %s: %w", legacy, err)
	}
	if err := r.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: legacy, Namespace: ns}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy Service %s: %w", legacy, err)
	}
	if err := r.Delete(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: legacy, Namespace: ns}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy Role %s: %w", legacy, err)
	}
	if err := r.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: legacy, Namespace: ns}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy RoleBinding %s: %w", legacy, err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) applyService(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      childName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = instanceLabels(inst.Name)
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		if r.OnOpenShift {
			svc.Annotations[openshiftServingCertAnnotation] = tlsSecretName(inst.Name)
		} else {
			delete(svc.Annotations, openshiftServingCertAnnotation)
		}
		clusterIP := svc.Spec.ClusterIP
		svc.Spec.Selector = serverLabels(inst.Name)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "https",
			Port:       proxyPort,
			TargetPort: intstr.FromString("https"),
			Protocol:   corev1.ProtocolTCP,
		}}
		if clusterIP != "" {
			svc.Spec.ClusterIP = clusterIP
		}
		return controllerutil.SetControllerReference(inst, svc, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply Service: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) applySandboxIngressNP(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name:      sandboxSAName(inst.Name),
		Namespace: inst.Namespace,
	}}
	port := intstr.FromInt32(sandboxAgentPort)
	protocol := corev1.ProtocolTCP
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = instanceLabels(inst.Name)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: sandboxLabels(inst.Name)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: serverLabels(inst.Name)},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: &protocol,
					Port:     &port,
				}},
			}},
		}
		return controllerutil.SetControllerReference(inst, np, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply sandbox ingress NetworkPolicy: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) applyDeployment(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, hmac *corev1.Secret) error {
	if r.Images.Server == "" {
		return fmt.Errorf("%s is empty", envRelatedImageServer)
	}
	if r.Images.KubeRBACProxy == "" {
		return fmt.Errorf("%s is empty", envRelatedImageKubeRBACProxy)
	}
	overlay := sandboxOverlay(inst.Spec.Sandbox, r.resolvedSandboxImage(inst.Spec.Sandbox))
	if overlay.Image == "" {
		return fmt.Errorf("sandbox image is empty: set spec.sandbox.image or %s", envRelatedImageSandbox)
	}
	args, err := mcpServerArgs(inst, overlay)
	if err != nil {
		return err
	}

	desired := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      childName(inst.Name),
		Namespace: inst.Namespace,
		Labels:    instanceLabels(inst.Name),
	}}
	replicas := replicasOrDefault(inst.Spec.Replicas)
	desired.Spec.Replicas = &replicas
	desired.Spec.Selector = &metav1.LabelSelector{MatchLabels: serverLabels(inst.Name)}
	desired.Spec.Template = r.mcpPodTemplate(inst, args, hmac)
	if err = controllerutil.SetControllerReference(inst, desired, r.Scheme); err != nil {
		return fmt.Errorf("deployment ownerRef: %w", err)
	}
	NormalizeDeployment(desired)

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      desired.Name,
		Namespace: desired.Namespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if deploy.Labels == nil {
			deploy.Labels = map[string]string{}
		}
		maps.Copy(deploy.Labels, desired.Labels)
		deploy.Spec = desired.Spec
		deploy.OwnerReferences = desired.OwnerReferences
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply Deployment: %w", err)
	}
	return nil
}

func mcpServerArgs(inst *climcpv1alpha1.CliMcpInstance, overlay session.SandboxConfig) ([]string, error) {
	args := []string{
		"--transport", "http",
		"--stateless",
		"--address", mcpListenAddr,
		"--namespace", inst.Namespace,
		"--instance-name", inst.Name,
		"--sandbox-image", overlay.Image,
		"--hmac-key-file", hmacMountPath + "/" + hmacSecretKey,
		"--kubeconfig-secret", kubeconfigSecretName(inst.Name),
		"--sandbox-service-account", sandboxSAName(inst.Name),
		"--sandbox-cpu-request", overlay.CPURequest,
		"--sandbox-cpu-limit", overlay.CPULimit,
		"--sandbox-memory-request", overlay.MemoryRequest,
		"--sandbox-memory-limit", overlay.MemoryLimit,
	}
	if overlay.ImagePullPolicy != "" {
		args = append(args, "--sandbox-image-pull-policy", string(overlay.ImagePullPolicy))
	}
	if len(overlay.Env) > 0 {
		raw, err := json.Marshal(overlay.Env)
		if err != nil {
			return nil, fmt.Errorf("marshal spec.sandbox.env: %w", err)
		}
		args = append(args, "--sandbox-env", string(raw))
	}
	return args, nil
}

func (r *CliMcpInstanceReconciler) mcpPodTemplate(inst *climcpv1alpha1.CliMcpInstance, args []string, hmac *corev1.Secret) corev1.PodTemplateSpec {
	runAsNonRoot := true
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: serverPodLabels(inst.Name),
			Annotations: map[string]string{
				hmacRVAnnotation: hmac.ResourceVersion,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: mcpServerSAName(inst.Name),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{
				r.mcpContainer(inst, args),
				kubeRBACProxyContainer(r.Images.KubeRBACProxy),
			},
			Volumes: []corev1.Volume{
				{
					Name: hmacVolumeName,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: hmacSecretName(inst.Name),
							Items: []corev1.KeyToPath{{
								Key:  hmacSecretKey,
								Path: hmacSecretKey,
							}},
						},
					},
				},
				{
					Name: tlsVolumeName,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: tlsSecretName(inst.Name),
						},
					},
				},
			},
		},
	}
}

func (r *CliMcpInstanceReconciler) mcpContainer(inst *climcpv1alpha1.CliMcpInstance, args []string) corev1.Container {
	allowPrivEsc := false
	c := corev1.Container{
		Name:  serverContainerName,
		Image: r.Images.Server,
		Args:  args,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &allowPrivEsc,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      hmacVolumeName,
			MountPath: hmacMountPath,
			ReadOnly:  true,
		}},
		ReadinessProbe: loopbackLiveProbe(2),
		LivenessProbe:  loopbackLiveProbe(15),
	}
	if inst.Spec.ServerContainer != nil {
		c.Resources = inst.Spec.ServerContainer.Resources
		c.ImagePullPolicy = inst.Spec.ServerContainer.ImagePullPolicy
	}
	return c
}

func loopbackLiveProbe(initialDelay int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/sh", "-c", "echo GET /live >/dev/tcp/127.0.0.1/8080"},
			},
		},
		InitialDelaySeconds: initialDelay,
		TimeoutSeconds:      2,
		PeriodSeconds:       10,
	}
}

func kubeRBACProxyContainer(image string) corev1.Container {
	allowPrivEsc := false
	https := intstr.FromString("https")
	return corev1.Container{
		Name:  proxyContainerName,
		Image: image,
		Args: []string{
			"--secure-listen-address=0.0.0.0:8443",
			"--upstream=http://127.0.0.1:8080/",
			"--tls-cert-file=" + tlsMountPath + "/tls.crt",
			"--tls-private-key-file=" + tlsMountPath + "/tls.key",
			"--allow-paths=/mcp,/metrics,/live,/health,/sessions,/sessions/*",
		},
		Ports: []corev1.ContainerPort{{
			Name:          "https",
			ContainerPort: proxyPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &allowPrivEsc,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      tlsVolumeName,
			MountPath: tlsMountPath,
			ReadOnly:  true,
		}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: https},
			},
			InitialDelaySeconds: 2,
			PeriodSeconds:       10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: https},
			},
			InitialDelaySeconds: 15,
			PeriodSeconds:       20,
		},
	}
}
