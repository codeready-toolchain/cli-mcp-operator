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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/kubeconfig"
)

func TestSandboxEgressIsInstanceScoped(t *testing.T) {
	t.Parallel()
	oc := sandboxEgress("oc")
	aws := sandboxEgress("aws")
	require.NotEmpty(t, oc)
	assert.Equal(t, proxyLabels("oc"), oc[0].To[0].PodSelector.MatchLabels)
	assert.Equal(t, proxyLabels("aws"), aws[0].To[0].PodSelector.MatchLabels)
	assert.NotEqual(t, oc[0].To[0].PodSelector.MatchLabels, aws[0].To[0].PodSelector.MatchLabels)

	proxyNP := proxyIngressSpec("oc")
	assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, proxyNP.PolicyTypes)
	assert.Equal(t, sandboxLabels("oc"), proxyNP.Ingress[0].From[0].PodSelector.MatchLabels)
	assert.NotEqual(t, proxyIngressSpec("aws").PodSelector.MatchLabels, proxyNP.PodSelector.MatchLabels)
}

func TestSandboxEgressDNSPeers(t *testing.T) {
	t.Parallel()
	rules := sandboxEgress("oc")
	require.Len(t, rules, 4)

	kubeDNS := rules[1].To[0]
	require.NotNil(t, kubeDNS.NamespaceSelector)
	require.NotNil(t, kubeDNS.PodSelector)
	assert.Equal(t, "kube-system", kubeDNS.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.Equal(t, "kube-dns", kubeDNS.PodSelector.MatchLabels["k8s-app"])

	openshiftDNS := rules[2].To[0]
	assert.Equal(t, "openshift-dns", openshiftDNS.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.Equal(t, "default", openshiftDNS.PodSelector.MatchLabels["dns.operator.openshift.io/daemonset-dns"])

	block := rules[3].To[0].IPBlock
	require.NotNil(t, block)
	assert.Equal(t, "169.254.20.10/32", block.CIDR)
	assert.Empty(t, block.Except)
	for _, rule := range rules {
		for _, peer := range rule.To {
			if peer.IPBlock == nil {
				continue
			}
			assert.NotEqual(t, "0.0.0.0/0", peer.IPBlock.CIDR)
		}
	}
	var dns53 int
	for _, port := range rules[3].Ports {
		require.NotNil(t, port.Port)
		if port.Port.IntVal == 53 {
			dns53++
		}
		assert.NotEqual(t, int32(5353), port.Port.IntVal)
	}
	assert.Equal(t, 2, dns53)
}

func TestEndpointsIgnoreNotReady(t *testing.T) {
	t.Parallel()
	assert.False(t, endpointsHaveAddresses(&corev1.Endpoints{}))
	assert.False(t, endpointsHaveAddresses(&corev1.Endpoints{
		Subsets: []corev1.EndpointSubset{{
			NotReadyAddresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}},
		}},
	}))
	assert.True(t, endpointsHaveAddresses(&corev1.Endpoints{
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}},
		}},
	}))
}

func TestApplyProxyMountsNamedSecretAndIgnoresTokenRotation(t *testing.T) {
	t.Parallel()
	scheme := proxyApplyScheme(t)
	inst := testInstance("oc", "ns")
	inst.Spec.Proxy.Targets = []climcpv1alpha1.ProxyTarget{{
		Type:       climcpv1alpha1.ProxyTargetKubernetes,
		SecretName: "custom-kube",
	}}
	kube := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-kube", Namespace: "ns"},
		Data:       map[string][]byte{kubeconfigDataKey: []byte(tokenKubeconfig)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst.DeepCopy(), kube).Build()
	r := &CliMcpInstanceReconciler{Client: c, Scheme: scheme, Images: testImages()}
	require.NoError(t, r.applyProxy(t.Context(), inst))

	deploy := &appsv1.Deployment{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: proxyName("oc"), Namespace: "ns"}, deploy))
	assert.Equal(t, "custom-kube", kubeconfigVolumeSecret(t, deploy))
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "custom-kube", Namespace: "ns"}, kube))
	firstStamp := deploy.Spec.Template.Annotations[kubeconfigRVAnnotation]
	assert.Equal(t, kube.ResourceVersion, firstStamp)

	cm := &corev1.ConfigMap{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: proxyName("oc"), Namespace: "ns"}, cm))
	firstRV := cm.ResourceVersion
	assert.NotContains(t, cm.Data[kubeconfigDataKey], "test-token")
	assert.Contains(t, cm.Data[kubeconfigDataKey], kubeconfig.DummyToken)

	kube.Data[kubeconfigDataKey] = []byte(strings.Replace(tokenKubeconfig, "token: test-token", "token: rotated-token", 1))
	require.NoError(t, c.Update(t.Context(), kube))
	require.NoError(t, r.applyProxy(t.Context(), inst))

	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: proxyName("oc"), Namespace: "ns"}, cm))
	assert.Equal(t, firstRV, cm.ResourceVersion)
	assert.NotContains(t, cm.Data[kubeconfigDataKey], "rotated-token")
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: proxyName("oc"), Namespace: "ns"}, deploy))
	assert.Equal(t, kube.ResourceVersion, deploy.Spec.Template.Annotations[kubeconfigRVAnnotation])
	assert.NotEqual(t, firstStamp, deploy.Spec.Template.Annotations[kubeconfigRVAnnotation])
}

func TestEmptyProxyCAIsNotRegenerated(t *testing.T) {
	t.Parallel()
	scheme := proxyApplyScheme(t)
	inst := testInstance("oc", "ns")
	inst.Spec.Proxy = kubernetesProxySpec()
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: proxyCASecretName("oc"), Namespace: "ns"},
		Data:       map[string][]byte{caCertKey: []byte("  "), caKeyKey: {}},
	}
	kube := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: kubeconfigSecretName("oc"), Namespace: "ns"},
		Data:       map[string][]byte{kubeconfigDataKey: []byte(tokenKubeconfig)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst.DeepCopy(), ca, kube).Build()
	r := &CliMcpInstanceReconciler{Client: c, Scheme: scheme, Images: testImages()}
	require.NoError(t, r.applyProxy(t.Context(), inst))

	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: proxyCASecretName("oc"), Namespace: "ns"}, ca))
	assert.Equal(t, []byte("  "), ca.Data[caCertKey])
	assert.Empty(t, ca.Data[caKeyKey])
	err := c.Get(t.Context(), types.NamespacedName{Name: proxyName("oc"), Namespace: "ns"}, &appsv1.Deployment{})
	assert.True(t, apierrors.IsNotFound(err))

	invalid, msg := r.invalidRequiredSecretKeys(t.Context(), inst, &corev1.Secret{
		Data: map[string][]byte{hmacSecretKey: []byte("key")},
	})
	assert.True(t, invalid)
	assert.Contains(t, msg, proxyCASecretName("oc")+"/"+caCertKey)
	assert.Contains(t, msg, proxyCASecretName("oc")+"/"+caKeyKey)
}

func proxyApplyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, climcpv1alpha1.AddToScheme(scheme))
	return scheme
}

func kubeconfigVolumeSecret(t *testing.T, deploy *appsv1.Deployment) string {
	t.Helper()
	for _, vol := range deploy.Spec.Template.Spec.Volumes {
		if vol.Name == "kubeconfig" && vol.Secret != nil {
			return vol.Secret.SecretName
		}
	}
	t.Fatal("kubeconfig volume not found")
	return ""
}
