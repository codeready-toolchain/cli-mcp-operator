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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConditionReady is the aggregate instance condition.
	ConditionReady = "Ready"
	// ConditionWarmPoolReady is the strict unassigned pool count.
	ConditionWarmPoolReady = "WarmPoolReady"

	ReasonReady                 = "Ready"
	ReasonReconciling           = "Reconciling"
	ReasonSecretsNotFound       = "SecretsNotFound"
	ReasonSecretKeysInvalid     = "SecretKeysInvalid"
	ReasonKubeconfigInvalid     = "KubeconfigInvalid"
	ReasonDeploymentUnavailable = "DeploymentUnavailable"
	ReasonChildrenNotReady      = "ChildrenNotReady"
	ReasonWarmPoolNotReady      = "WarmPoolNotReady"
	ReasonWarmPoolUnhealthy     = "WarmPoolUnhealthy"
)

// ProxyTargetType is spec.proxy.targets[].type.
type ProxyTargetType string

const (
	// ProxyTargetKubernetes injects the investigation kubeconfig for that host:port.
	ProxyTargetKubernetes ProxyTargetType = "kubernetes"
	// ProxyTargetAllowlist MITMs the listed hosts and injects nothing.
	ProxyTargetAllowlist ProxyTargetType = "allowlist"
)

// CliMcpInstanceSpec defines the desired state of one MCP sandbox class / instance.
type CliMcpInstanceSpec struct {
	// Replicas is the number of MCP server pods.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`

	// Sandbox is the sandbox class (image + config) for this instance.
	// +kubebuilder:default={}
	Sandbox SandboxSpec `json:"sandbox,omitempty"`

	// ServerContainer optionally sets resources / imagePullPolicy on the MCP
	// container. The MCP image is always RELATED_IMAGE_SERVER, not a spec field.
	// +optional
	ServerContainer *ServerContainerSpec `json:"serverContainer,omitempty"`

	// Proxy is the credential-isolating proxy for this instance.
	// Required. Omitting it is invalid; there is no proxy-less instance.
	// +kubebuilder:validation:Required
	Proxy ProxySpec `json:"proxy"`
}

// ProxySpec is the proxy API. Image, replicas, and resources are operator
// constants, not spec fields.
type ProxySpec struct {
	// Targets is the host allowlist and credential injectors for this instance.
	// At most one kubernetes target. Mixing kubernetes and allowlist is this
	// instance's union.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	Targets []ProxyTarget `json:"targets"`
}

// ProxyTarget is one kubernetes kubeconfig or one allowlist.
type ProxyTarget struct {
	// Type selects kubernetes (inject) or allowlist (strip only).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=kubernetes;allowlist
	Type ProxyTargetType `json:"type"`

	// SecretName is the investigation kubeconfig Secret. Kubernetes only.
	// Empty means cli-mcp-<instance>-kubeconfig. The key is kubeconfig.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	SecretName string `json:"secretName,omitempty"`

	// Domains are literal hosts or host:port values. Allowlist only.
	// A bare host means port 443 when routes are built. No wildcards and no
	// leading-dot suffixes.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +listType=set
	Domains []string `json:"domains,omitempty"`
}

// SandboxSpec is the user-mergeable sandbox class. Operator-owned pod fields
// (SA, automount, labels, probes, kubeconfig mount, security context) are not
// spec fields.
type SandboxSpec struct {
	// Image is the sandbox class image. Empty uses RELATED_IMAGE_SANDBOX.
	// +optional
	Image string `json:"image,omitempty"`

	// IdleTimeout is how long an assigned session may be idle before the
	// operator GCs it. Consumed by the operator; not passed as an MCP flag.
	// +kubebuilder:default="30m"
	IdleTimeout metav1.Duration `json:"idleTimeout,omitempty"`

	// WarmPoolSize is the desired unassigned pool the operator maintains.
	// Default 0 (on-demand create only). Changing this field does not roll MCP.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	WarmPoolSize int32 `json:"warmPoolSize,omitempty"`

	// Resources for sandbox pods. Empty/omitted uses DefaultConfig
	// 100m/500m/128Mi/512Mi, not BestEffort.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env overlay for sandbox pods, including valueFrom. Operator-owned
	// names (KUBECONFIG, HOME, SANDBOX_AUTH_TOKEN) are ignored by the builder.
	// Extra Secrets referenced here are not Ready gates.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// ImagePullPolicy for sandbox pods.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// ServerContainerSpec is optional MCP-container-only overlay.
type ServerContainerSpec struct {
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// CliMcpInstanceStatus is observed instance state.
type CliMcpInstanceStatus struct {
	// WarmPoolReady is the number of unassigned Ready pool pods.
	// Always published.
	WarmPoolReady int32 `json:"warmPoolReady"`

	// WarmPoolDesired is spec.sandbox.warmPoolSize. Always published.
	WarmPoolDesired int32 `json:"warmPoolDesired"`

	// ResolvedSandboxImage is spec.sandbox.image or RELATED_IMAGE_SANDBOX.
	// +optional
	ResolvedSandboxImage string `json:"resolvedSandboxImage,omitempty"`

	// ClientServiceAccount is the operator-managed ServiceAccount that is allowed
	// to call this instance through kube-rbac-proxy. Mint a token with
	// `kubectl create token <name> -n <namespace>` (or `oc create token`).
	// The operator does not mint or store that token.
	// +optional
	ClientServiceAccount string `json:"clientServiceAccount,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 44",message="metadata.name must be at most 44 characters so cli-mcp-<name>-kubeconfig is a valid DNS-1123 label"
// +kubebuilder:validation:XValidation:rule="self.spec.proxy.targets.filter(t, t.type == 'kubernetes').size() <= 1",message="at most one kubernetes target"
// +kubebuilder:validation:XValidation:rule="self.spec.proxy.targets.all(t, t.type != 'kubernetes' || !has(t.domains))",message="kubernetes target must not set domains"
// +kubebuilder:validation:XValidation:rule="self.spec.proxy.targets.all(t, t.type == 'kubernetes' || !has(t.secretName))",message="secretName is only valid on a kubernetes target"
// +kubebuilder:validation:XValidation:rule="self.spec.proxy.targets.all(t, t.type != 'allowlist' || (has(t.domains) && size(t.domains) >= 1))",message="allowlist target requires domains"
// +kubebuilder:validation:XValidation:rule="self.spec.proxy.targets.all(t, t.type != 'allowlist' || t.domains.all(d, !d.contains('*') && !d.startsWith('.') && d.matches('^(\\\\[[0-9A-Fa-f:.]+\\\\]|[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\\\\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*)(:([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?$')))",message="domains must be literal hosts or host:port, including [ipv6]:port"

// CliMcpInstance is one MCP class/instance (one sandbox image + config, one MCP Deployment).
type CliMcpInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CliMcpInstanceSpec   `json:"spec,omitempty"`
	Status CliMcpInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CliMcpInstanceList contains a list of CliMcpInstance.
type CliMcpInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CliMcpInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CliMcpInstance{}, &CliMcpInstanceList{})
}
