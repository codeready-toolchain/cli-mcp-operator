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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"time"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	credentialProxyContainer = "proxy"
	proxyConfigDir           = "/etc/cli-mcp/proxy"
	proxyCADir               = "/etc/cli-mcp/proxy-ca"
	proxyKubeDir             = "/etc/kube"

	nodeLocalDNS = "169.254.20.10/32"
)

var (
	proxyCPURequest    = resource.MustParse("20m")
	proxyCPULimit      = resource.MustParse("200m")
	proxyMemoryRequest = resource.MustParse("64Mi")
	proxyMemoryLimit   = resource.MustParse("256Mi")
)

type proxyRouteFile struct {
	Routes []kubeconfig.ProxyRoute `json:"routes"`
}

type proxyBuild struct {
	routes []kubeconfig.ProxyRoute
	dummy  []byte
	kube   *corev1.Secret
	ca     *corev1.Secret
	cm     *corev1.ConfigMap
}

func (r *CliMcpInstanceReconciler) applyProxy(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	if err := r.applyProxySA(ctx, inst); err != nil {
		return err
	}
	if err := r.applyProxyService(ctx, inst); err != nil {
		return err
	}
	if err := r.applyProxyNP(ctx, inst); err != nil {
		return err
	}
	ca, err := r.ensureProxyCA(ctx, inst)
	if err != nil {
		return err
	}
	if r.Images.Proxy == "" {
		return fmt.Errorf("%s is empty", envRelatedImageProxy)
	}
	build, err := r.loadProxyBuild(ctx, inst, ca)
	if err != nil {
		return err
	}
	if !caUsable(ca) || len(build.routes) == 0 {
		// A running proxy already loaded the previous token. Empty routes cannot
		// replace it: the new pod would never become Ready, and maxUnavailable is 0.
		return r.quiesceUnroutedProxy(ctx, inst)
	}
	cm, err := r.applyProxyConfigMap(ctx, inst, build)
	if err != nil {
		return err
	}
	build.cm = cm
	return r.applyProxyDeployment(ctx, inst, build)
}

// quiesceUnroutedProxy stops a proxy that no longer has routes and drops the
// published dummy kubeconfig so the pool gate closes. It does not create
// children that were never published.
func (r *CliMcpInstanceReconciler) quiesceUnroutedProxy(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	if err := r.dropPublishedKubeconfig(ctx, inst); err != nil {
		return err
	}
	return r.scaleProxyToZero(ctx, inst)
}

func (r *CliMcpInstanceReconciler) dropPublishedKubeconfig(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: proxyName(inst.Name)}, cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get proxy configmap: %w", err)
	}
	raw, err := json.Marshal(proxyRouteFile{Routes: []kubeconfig.ProxyRoute{}})
	if err != nil {
		return fmt.Errorf("marshal empty proxy routes: %w", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	if cm.Data[proxyConfigDataKey] == string(raw) && cm.Data[kubeconfigDataKey] == "" {
		return nil
	}
	cm.Data = map[string]string{proxyConfigDataKey: string(raw)}
	if err := r.Update(ctx, cm); err != nil {
		return fmt.Errorf("clear proxy configmap: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) scaleProxyToZero(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	deploy := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: proxyName(inst.Name)}, deploy)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get proxy deployment: %w", err)
	}
	if !quiesceProxyDeployment(deploy) {
		return nil
	}
	if err := r.Update(ctx, deploy); err != nil {
		return fmt.Errorf("scale proxy deployment to zero: %w", err)
	}
	return nil
}

func quiesceProxyDeployment(deploy *appsv1.Deployment) bool {
	changed := false
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 0 {
		zero := int32(0)
		deploy.Spec.Replicas = &zero
		changed = true
	}
	if deploy.Spec.Template.Annotations != nil {
		if _, ok := deploy.Spec.Template.Annotations[kubeconfigRVAnnotation]; ok {
			delete(deploy.Spec.Template.Annotations, kubeconfigRVAnnotation)
			changed = true
		}
	}
	spec := &deploy.Spec.Template.Spec
	volumes := make([]corev1.Volume, 0, len(spec.Volumes))
	for _, vol := range spec.Volumes {
		if vol.Name == "kubeconfig" {
			changed = true
			continue
		}
		volumes = append(volumes, vol)
	}
	if len(volumes) != len(spec.Volumes) {
		spec.Volumes = volumes
	}
	for i := range spec.Containers {
		mounts := make([]corev1.VolumeMount, 0, len(spec.Containers[i].VolumeMounts))
		dropped := false
		for _, mount := range spec.Containers[i].VolumeMounts {
			if mount.Name == "kubeconfig" {
				dropped = true
				changed = true
				continue
			}
			mounts = append(mounts, mount)
		}
		if dropped {
			spec.Containers[i].VolumeMounts = mounts
		}
	}
	return changed
}

func (r *CliMcpInstanceReconciler) applyProxySA(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      proxyName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = instanceLabels(inst.Name)
		automount := false
		sa.AutomountServiceAccountToken = &automount
		return controllerutil.SetControllerReference(inst, sa, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply proxy SA: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) applyProxyService(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      proxyName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = instanceLabels(inst.Name)
		clusterIP := svc.Spec.ClusterIP
		svc.Spec.Selector = proxyLabels(inst.Name)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       session.ProxyListenPort,
			TargetPort: intstr.FromInt32(session.ProxyListenPort),
			Protocol:   corev1.ProtocolTCP,
		}}
		if clusterIP != "" {
			svc.Spec.ClusterIP = clusterIP
		}
		return controllerutil.SetControllerReference(inst, svc, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply proxy Service: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) applyProxyNP(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name:      proxyName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = instanceLabels(inst.Name)
		np.Spec = proxyIngressSpec(inst.Name)
		return controllerutil.SetControllerReference(inst, np, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply proxy NetworkPolicy: %w", err)
	}
	return nil
}

func proxyIngressSpec(instance string) networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: proxyLabels(instance)},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: sandboxLabels(instance)},
			}},
			Ports: npPorts(portPair{port: session.ProxyListenPort, proto: corev1.ProtocolTCP}),
		}},
	}
}

func (r *CliMcpInstanceReconciler) ensureProxyCA(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (*corev1.Secret, error) {
	nn := types.NamespacedName{Namespace: inst.Namespace, Name: proxyCASecretName(inst.Name)}
	secret := &corev1.Secret{}
	err := r.Get(ctx, nn, secret)
	if err == nil {
		if metaErr := r.adoptProxyCAMetadata(ctx, inst, secret); metaErr != nil {
			return nil, metaErr
		}
		return secret, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get proxy CA secret: %w", err)
	}
	certPEM, keyPEM, err := generateProxyCA()
	if err != nil {
		return nil, err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nn.Name,
			Namespace: nn.Namespace,
			Labels:    instanceLabels(inst.Name),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			caCertKey: certPEM,
			caKeyKey:  keyPEM,
		},
	}
	if err := controllerutil.SetControllerReference(inst, secret, r.Scheme); err != nil {
		return nil, fmt.Errorf("proxy CA ownerRef: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("create proxy CA secret: %w", err)
	}
	if err := r.Get(ctx, nn, secret); err != nil {
		return nil, fmt.Errorf("get proxy CA secret after create: %w", err)
	}
	return secret, nil
}

func (r *CliMcpInstanceReconciler) adoptProxyCAMetadata(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, secret *corev1.Secret) error {
	before := secret.DeepCopy()
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[session.LabelInstance] = inst.Name
	if err := controllerutil.SetControllerReference(inst, secret, r.Scheme); err != nil {
		return fmt.Errorf("proxy CA ownerRef: %w", err)
	}
	if equality.Semantic.DeepEqual(before.Labels, secret.Labels) &&
		equality.Semantic.DeepEqual(before.OwnerReferences, secret.OwnerReferences) {
		return nil
	}
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("update proxy CA metadata: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) loadProxyBuild(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, ca *corev1.Secret) (proxyBuild, error) {
	build := proxyBuild{ca: ca}
	var routes []kubeconfig.ProxyRoute
	for _, target := range proxyTargets(inst) {
		switch target.Type {
		case climcpv1alpha1.ProxyTargetAllowlist:
			part, err := kubeconfig.BuildAllowlistRoutes(target.Domains)
			if err != nil {
				return build, fmt.Errorf("allowlist routes: %w", err)
			}
			routes = append(routes, part...)
		case climcpv1alpha1.ProxyTargetKubernetes:
			part, dummy, secret, err := r.kubernetesRoutes(ctx, inst, ca)
			if err != nil {
				return build, err
			}
			routes = append(routes, part...)
			build.dummy = dummy
			build.kube = secret
		default:
			return build, fmt.Errorf("unknown proxy target type %q", target.Type)
		}
	}
	merged, err := mergeRoutes(routes)
	if err != nil {
		return build, err
	}
	build.routes = merged
	return build, nil
}

func (r *CliMcpInstanceReconciler) kubernetesRoutes(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, ca *corev1.Secret) ([]kubeconfig.ProxyRoute, []byte, *corev1.Secret, error) {
	name, ok := effectiveKubeconfigSecretName(inst)
	if !ok {
		return nil, nil, nil, nil
	}
	secret, err := r.getSecret(ctx, inst.Namespace, name)
	if err != nil {
		return nil, nil, nil, err
	}
	if secret == nil || !secretKeyNonEmpty(secret, kubeconfigDataKey) || !caUsable(ca) {
		return nil, nil, nil, nil
	}
	data := secret.Data[kubeconfigDataKey]
	if _, validateErr := kubeconfig.Validate(data); validateErr != nil {
		// Invalid kubeconfig is a status condition. Skip routes until it parses.
		return nil, nil, nil, nil //nolint:nilerr // readyGate reports KubeconfigInvalid
	}
	dummy, err := kubeconfig.Sanitize(data, ca.Data[caCertKey])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sanitize kubeconfig: %w", err)
	}
	routes, err := kubeconfig.BuildProxyRoutes(data, proxyKubeconfigPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("kubernetes routes: %w", err)
	}
	return routes, dummy, secret, nil
}

func (r *CliMcpInstanceReconciler) applyProxyConfigMap(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, build proxyBuild) (*corev1.ConfigMap, error) {
	raw, err := json.Marshal(proxyRouteFile{Routes: build.routes})
	if err != nil {
		return nil, fmt.Errorf("marshal proxy routes: %w", err)
	}
	desired := map[string]string{proxyConfigDataKey: string(raw)}
	if len(build.dummy) > 0 {
		desired[kubeconfigDataKey] = string(build.dummy)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      proxyName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = instanceLabels(inst.Name)
		cm.Data = desired
		return controllerutil.SetControllerReference(inst, cm, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("apply proxy ConfigMap: %w", err)
	}
	return cm, nil
}

func (r *CliMcpInstanceReconciler) applyProxyDeployment(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, build proxyBuild) error {
	desired := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      proxyName(inst.Name),
		Namespace: inst.Namespace,
		Labels:    instanceLabels(inst.Name),
	}}
	replicas := int32(1)
	desired.Spec.Replicas = &replicas
	desired.Spec.Selector = &metav1.LabelSelector{MatchLabels: proxyLabels(inst.Name)}
	desired.Spec.Strategy = proxyStrategy()
	desired.Spec.Template = proxyPodTemplate(r.Images.Proxy, inst.Name, build)
	if err := controllerutil.SetControllerReference(inst, desired, r.Scheme); err != nil {
		return fmt.Errorf("proxy deployment ownerRef: %w", err)
	}
	NormalizeDeployment(desired)

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      desired.Name,
		Namespace: desired.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if deploy.Labels == nil {
			deploy.Labels = map[string]string{}
		}
		maps.Copy(deploy.Labels, desired.Labels)
		deploy.Spec = desired.Spec
		deploy.OwnerReferences = desired.OwnerReferences
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply proxy Deployment: %w", err)
	}
	return nil
}

func proxyStrategy() appsv1.DeploymentStrategy {
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
}

func proxyPodTemplate(image, instance string, build proxyBuild) corev1.PodTemplateSpec {
	runAsNonRoot := true
	automount := false
	allowPrivEsc := false
	grace := proxyTerminationGrace
	annotations := map[string]string{
		proxyRoutesRVAnnotation: build.cm.ResourceVersion,
		proxyCARVAnnotation:     build.ca.ResourceVersion,
	}
	if build.kube != nil {
		annotations[kubeconfigRVAnnotation] = build.kube.ResourceVersion
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      proxyLabels(instance),
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:            proxyName(instance),
			AutomountServiceAccountToken:  &automount,
			TerminationGracePeriodSeconds: &grace,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:  credentialProxyContainer,
				Image: image,
				Args: []string{
					"--config=" + proxyConfigDir + "/" + proxyConfigDataKey,
					"--ca-cert=" + proxyCADir + "/" + caCertKey,
					"--ca-key=" + proxyCADir + "/" + caKeyKey,
					fmt.Sprintf("--listen=:%d", session.ProxyListenPort),
				},
				Ports: []corev1.ContainerPort{{
					Name:          "http",
					ContainerPort: session.ProxyListenPort,
					Protocol:      corev1.ProtocolTCP,
				}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    proxyCPURequest,
						corev1.ResourceMemory: proxyMemoryRequest,
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    proxyCPULimit,
						corev1.ResourceMemory: proxyMemoryLimit,
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &allowPrivEsc,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				ReadinessProbe: proxyTCPProbe(),
				LivenessProbe:  proxyTCPProbe(),
				Lifecycle: &corev1.Lifecycle{
					PreStop: &corev1.LifecycleHandler{
						Sleep: &corev1.SleepAction{Seconds: proxyPreStopSeconds},
					},
				},
				VolumeMounts: proxyVolumeMounts(build.kube != nil),
			}},
			Volumes: proxyVolumes(instance, kubeSecretName(build)),
		},
	}
}

func proxyTCPProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(session.ProxyListenPort)},
		},
	}
}

func proxyVolumeMounts(kube bool) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "proxy-config", MountPath: proxyConfigDir, ReadOnly: true},
		{Name: "proxy-ca", MountPath: proxyCADir, ReadOnly: true},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if kube {
		mounts = append(mounts, corev1.VolumeMount{Name: "kubeconfig", MountPath: proxyKubeDir, ReadOnly: true})
	}
	return mounts
}

func kubeSecretName(build proxyBuild) string {
	if build.kube == nil {
		return ""
	}
	return build.kube.Name
}

func proxyVolumes(instance, kubeSecret string) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "proxy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: proxyName(instance)},
				},
			},
		},
		{
			Name: "proxy-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: proxyCASecretName(instance)},
			},
		},
		{
			Name: "tmp",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}
	if kubeSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "kubeconfig",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: kubeSecret,
					Items: []corev1.KeyToPath{{
						Key:  kubeconfigDataKey,
						Path: "config",
					}},
				},
			},
		})
	}
	return volumes
}

func sandboxEgress(instance string) []networkingv1.NetworkPolicyEgressRule {
	dnsPorts := npPorts(
		portPair{port: 53, proto: corev1.ProtocolUDP},
		portPair{port: 53, proto: corev1.ProtocolTCP},
		portPair{port: 5353, proto: corev1.ProtocolUDP},
		portPair{port: 5353, proto: corev1.ProtocolTCP},
	)
	return []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: proxyLabels(instance)},
			}},
			Ports: npPorts(portPair{port: session.ProxyListenPort, proto: corev1.ProtocolTCP}),
		},
		{
			To:    []networkingv1.NetworkPolicyPeer{dnsPeer("kube-system", "k8s-app", "kube-dns")},
			Ports: dnsPorts,
		},
		{
			To: []networkingv1.NetworkPolicyPeer{
				dnsPeer("openshift-dns", "dns.operator.openshift.io/daemonset-dns", "default"),
			},
			Ports: dnsPorts,
		},
		{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: nodeLocalDNS},
			}},
			Ports: npPorts(
				portPair{port: 53, proto: corev1.ProtocolUDP},
				portPair{port: 53, proto: corev1.ProtocolTCP},
			),
		},
	}
}

func dnsPeer(namespace, labelKey, labelValue string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			"kubernetes.io/metadata.name": namespace,
		}},
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			labelKey: labelValue,
		}},
	}
}

type portPair struct {
	port  int32
	proto corev1.Protocol
}

func npPorts(pairs ...portPair) []networkingv1.NetworkPolicyPort {
	out := make([]networkingv1.NetworkPolicyPort, 0, len(pairs))
	for _, pair := range pairs {
		p := intstr.FromInt32(pair.port)
		proto := pair.proto
		out = append(out, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p})
	}
	return out
}

func mergeRoutes(routes []kubeconfig.ProxyRoute) ([]kubeconfig.ProxyRoute, error) {
	seen := make(map[string]struct{}, len(routes))
	out := make([]kubeconfig.ProxyRoute, 0, len(routes))
	for _, route := range routes {
		if _, ok := seen[route.Domain]; ok {
			return nil, fmt.Errorf("duplicate proxy route %s", route.Domain)
		}
		seen[route.Domain] = struct{}{}
		out = append(out, route)
	}
	slices.SortFunc(out, func(a, b kubeconfig.ProxyRoute) int {
		if a.Domain < b.Domain {
			return -1
		}
		if a.Domain > b.Domain {
			return 1
		}
		return 0
	})
	return out, nil
}

func caUsable(secret *corev1.Secret) bool {
	return secretKeyNonEmpty(secret, caCertKey) && secretKeyNonEmpty(secret, caKeyKey)
}

func generateProxyCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate proxy CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate proxy CA serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "CLI MCP",
			Organization: []string{"CLI MCP"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create proxy CA certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal proxy CA key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, fmt.Errorf("encode proxy CA PEM")
	}
	return certPEM, keyPEM, nil
}

func (r *CliMcpInstanceReconciler) proxyPoolOpen(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (bool, error) {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: proxyName(inst.Name)}, cm)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get proxy configmap: %w", err)
	}
	if _, ok := effectiveKubeconfigSecretName(inst); ok && !secretKeyNonEmptyCM(cm, kubeconfigDataKey) {
		return false, nil
	}
	np := &networkingv1.NetworkPolicy{}
	err = r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: sandboxSAName(inst.Name)}, np)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get sandbox network policy: %w", err)
	}
	if !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		return false, nil
	}
	proxyNP := &networkingv1.NetworkPolicy{}
	err = r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: proxyName(inst.Name)}, proxyNP)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get proxy network policy: %w", err)
	}
	var sliceList discoveryv1.EndpointSliceList
	err = r.List(ctx, &sliceList,
		client.InNamespace(inst.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: proxyName(inst.Name)},
	)
	if err != nil {
		return false, fmt.Errorf("list proxy endpoint slices: %w", err)
	}
	return session.EndpointSlicesReady(sliceList.Items), nil
}

func secretKeyNonEmptyCM(cm *corev1.ConfigMap, key string) bool {
	if cm == nil || cm.Data == nil {
		return false
	}
	return cm.Data[key] != ""
}
