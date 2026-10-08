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

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
)

const (
	finalizerName = "cli-mcp.redhat.com/finalizer"

	hmacSecretKey     = "key"
	kubeconfigDataKey = "kubeconfig"
	tlsCertKey        = "tls.crt"
	tlsKeyKey         = "tls.key"

	hmacMountPath = "/var/run/cli-mcp/hmac"
	tlsMountPath  = "/etc/tls/private"
	krpMountPath  = "/etc/kube-rbac-proxy"

	hmacVolumeName = "hmac"
	tlsVolumeName  = "tls"
	krpVolumeName  = "kube-rbac-proxy-config"
	krpConfigKey   = "config.yaml"

	serverContainerName = "server"
	proxyContainerName  = "kube-rbac-proxy"

	mcpListenAddr    = "127.0.0.1:8080"
	proxyPort        = int32(8443)
	sandboxAgentPort = int32(8090)

	hmacRVAnnotation         = "cli-mcp.redhat.com/hmac-resource-version"
	krpRVAnnotation          = "cli-mcp.redhat.com/krp-resource-version"
	proxyRoutesRVAnnotation  = "cli-mcp.redhat.com/proxy-routes-resource-version"
	proxyCARVAnnotation      = "cli-mcp.redhat.com/proxy-ca-resource-version"
	kubeconfigRVAnnotation   = "cli-mcp.redhat.com/kubeconfig-resource-version"
	sandboxOverlayAnnotation = "cli-mcp.redhat.com/sandbox-overlay"

	caCertKey           = "ca.crt"
	caKeyKey            = "ca.key"
	proxyConfigDataKey  = "proxy-config.json"
	proxyKubeconfigPath = "/etc/kube/config"

	proxyPreStopSeconds   = int64(5)
	proxyTerminationGrace = int64(30)

	authDelegatorCRBName     = "cli-mcp-auth-delegator"
	authDelegatorClusterRole = "system:auth-delegator"

	openshiftServingCertAnnotation = "service.beta.openshift.io/serving-cert-secret-name"

	appNameLabel      = "app.kubernetes.io/name"
	appNameServer     = "cli-mcp-server"
	childNamePrefix   = "cli-mcp-"
	kubeconfigSuffix  = "-kubeconfig"
	tlsSuffix         = "-tls"
	hmacSuffix        = "-hmac"
	sandboxNameSuffix = "-sandbox"
	clientNameSuffix  = "-client"
	krpNameSuffix     = "-krp"
)

func childName(instance string) string {
	return childNamePrefix + instance
}

func hmacSecretName(instance string) string {
	return childNamePrefix + instance + hmacSuffix
}

func sandboxSAName(instance string) string {
	return childNamePrefix + instance + sandboxNameSuffix
}

func kubeconfigSecretName(instance string) string {
	return childNamePrefix + instance + kubeconfigSuffix
}

func tlsSecretName(instance string) string {
	return childNamePrefix + instance + tlsSuffix
}

func clientSAName(instance string) string {
	return childNamePrefix + instance + clientNameSuffix
}

func krpConfigMapName(instance string) string {
	return childNamePrefix + instance + krpNameSuffix
}

func proxyName(instance string) string {
	return childNamePrefix + instance + "-proxy"
}

func proxyCASecretName(instance string) string {
	return proxyName(instance) + "-ca"
}

func instanceLabels(instance string) map[string]string {
	return map[string]string{session.LabelInstance: instance}
}

func serverLabels(instance string) map[string]string {
	return map[string]string{
		session.LabelInstance:  instance,
		session.LabelComponent: session.ComponentServer,
	}
}

func serverPodLabels(instance string) map[string]string {
	labels := serverLabels(instance)
	labels[appNameLabel] = appNameServer
	return labels
}

func sandboxLabels(instance string) map[string]string {
	return map[string]string{
		session.LabelInstance:  instance,
		session.LabelComponent: session.ComponentSandbox,
	}
}

func proxyLabels(instance string) map[string]string {
	return map[string]string{
		session.LabelInstance:  instance,
		session.LabelComponent: session.ComponentProxy,
	}
}

// effectiveKubeconfigSecretName is the admin kubeconfig Secret for a kubernetes
// target. The bool is false when the instance has no kubernetes target.
// An empty secretName stays empty in the spec; the conventional name is computed here.
func proxyTargets(inst *climcpv1alpha1.CliMcpInstance) []climcpv1alpha1.ProxyTarget {
	if inst.Spec.Proxy == nil {
		return nil
	}
	return inst.Spec.Proxy.Targets
}

func effectiveKubeconfigSecretName(inst *climcpv1alpha1.CliMcpInstance) (string, bool) {
	for _, target := range proxyTargets(inst) {
		if target.Type != climcpv1alpha1.ProxyTargetKubernetes {
			continue
		}
		if target.SecretName != "" {
			return target.SecretName, true
		}
		return kubeconfigSecretName(inst.Name), true
	}
	return "", false
}

// instanceFromAdminSecret returns the CR name encoded in conventional
// admin/operator Secret names. Session auth Secrets are not matched.
func instanceFromAdminSecret(name string) (string, bool) {
	if !strings.HasPrefix(name, childNamePrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(name, childNamePrefix)
	for _, suffix := range []string{kubeconfigSuffix, tlsSuffix, hmacSuffix} {
		if strings.HasSuffix(rest, suffix) {
			return strings.TrimSuffix(rest, suffix), true
		}
	}
	return "", false
}
