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

	hmacVolumeName = "hmac"
	tlsVolumeName  = "tls"

	serverContainerName = "server"
	proxyContainerName  = "kube-rbac-proxy"

	mcpListenAddr    = "127.0.0.1:8080"
	proxyPort        = int32(8443)
	sandboxAgentPort = int32(8090)

	hmacRVAnnotation = "cli-mcp.redhat.com/hmac-resource-version"

	openshiftServingCertAnnotation = "service.beta.openshift.io/serving-cert-secret-name"

	appNameLabel      = "app.kubernetes.io/name"
	appNameServer     = "cli-mcp-server"
	childNamePrefix   = "cli-mcp-"
	kubeconfigSuffix  = "-kubeconfig"
	tlsSuffix         = "-tls"
	hmacSuffix        = "-hmac"
	sandboxNameSuffix = "-sandbox"
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
