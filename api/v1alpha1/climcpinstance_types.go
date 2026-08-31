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
	ReasonDeploymentUnavailable = "DeploymentUnavailable"
	ReasonChildrenNotReady      = "ChildrenNotReady"
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

	// WarmPoolSize is the desired unassigned pool. The operator does not
	// replenish the pool until Phase 5. Default 0 (on-demand create only).
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
	// Always published; pool replenishment is Phase 5.
	WarmPoolReady int32 `json:"warmPoolReady"`

	// WarmPoolDesired is spec.sandbox.warmPoolSize. Always published.
	WarmPoolDesired int32 `json:"warmPoolDesired"`

	// ResolvedSandboxImage is spec.sandbox.image or RELATED_IMAGE_SANDBOX.
	// +optional
	ResolvedSandboxImage string `json:"resolvedSandboxImage,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 44",message="metadata.name must be at most 44 characters so cli-mcp-<name>-kubeconfig is a valid DNS-1123 label"

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
