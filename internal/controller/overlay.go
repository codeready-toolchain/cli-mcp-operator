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
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

const (
	envRelatedImageServer        = "RELATED_IMAGE_SERVER"
	envRelatedImageSandbox       = "RELATED_IMAGE_SANDBOX"
	envRelatedImageKubeRBACProxy = "RELATED_IMAGE_KUBE_RBAC_PROXY"
	envRelatedImageProxy         = "RELATED_IMAGE_PROXY"

	defaultIdleTimeout = 30 * time.Minute
)

// Images are operand images from OLM relatedImages / operator env.
type Images struct {
	Server        string
	Sandbox       string
	KubeRBACProxy string
	Proxy         string
}

func ImagesFromEnv() Images {
	return Images{
		Server:        os.Getenv(envRelatedImageServer),
		Sandbox:       os.Getenv(envRelatedImageSandbox),
		KubeRBACProxy: os.Getenv(envRelatedImageKubeRBACProxy),
		Proxy:         os.Getenv(envRelatedImageProxy),
	}
}

func (r *CliMcpInstanceReconciler) resolvedSandboxImage(spec climcpv1alpha1.SandboxSpec) string {
	return cmp.Or(spec.Image, r.Images.Sandbox)
}

func sandboxOverlay(spec climcpv1alpha1.SandboxSpec, resolvedImage string) session.SandboxConfig {
	defaults := session.DefaultConfig()
	return session.SandboxConfig{
		Image:           resolvedImage,
		CPURequest:      quantityOr(spec.Resources.Requests, corev1.ResourceCPU, defaults.CPURequest),
		CPULimit:        quantityOr(spec.Resources.Limits, corev1.ResourceCPU, defaults.CPULimit),
		MemoryRequest:   quantityOr(spec.Resources.Requests, corev1.ResourceMemory, defaults.MemoryRequest),
		MemoryLimit:     quantityOr(spec.Resources.Limits, corev1.ResourceMemory, defaults.MemoryLimit),
		ImagePullPolicy: spec.ImagePullPolicy,
		Env:             spec.Env,
		AgentPort:       defaults.AgentPort,
	}
}

func (r *CliMcpInstanceReconciler) poolSandboxConfig(inst *climcpv1alpha1.CliMcpInstance) session.SandboxConfig {
	cfg := sandboxOverlay(inst.Spec.Sandbox, r.resolvedSandboxImage(inst.Spec.Sandbox))
	cfg.InstanceName = inst.Name
	cfg.Namespace = inst.Namespace
	cfg.ServiceAccountName = sandboxSAName(inst.Name)
	cfg.ProxyService = proxyName(inst.Name)
	cfg.ProxyCASecret = proxyCASecretName(inst.Name)
	if _, ok := effectiveKubeconfigSecretName(inst); ok {
		cfg.DummyKubeconfigConfigMap = proxyName(inst.Name)
	}
	return cfg
}

func (r *CliMcpInstanceReconciler) overlayInputs(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (session.SandboxConfig, []byte, []byte, error) {
	cfg := r.poolSandboxConfig(inst)
	var dummy, ca []byte
	if cfg.DummyKubeconfigConfigMap != "" {
		cm := &corev1.ConfigMap{}
		err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: cfg.DummyKubeconfigConfigMap}, cm)
		if err != nil && !apierrors.IsNotFound(err) {
			return cfg, nil, nil, fmt.Errorf("get dummy kubeconfig: %w", err)
		}
		if err == nil && cm.Data[kubeconfigDataKey] != "" {
			dummy = []byte(cm.Data[kubeconfigDataKey])
		}
	}
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: cfg.ProxyCASecret}, sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return cfg, nil, nil, fmt.Errorf("get proxy CA: %w", err)
	}
	if err == nil {
		if v := bytes.TrimSpace(sec.Data[caCertKey]); len(v) > 0 {
			ca = bytes.Clone(v)
		}
	}
	return cfg, dummy, ca, nil
}

type overlayFingerprint struct {
	Image           string            `json:"image"`
	CPURequest      string            `json:"cpuRequest"`
	CPULimit        string            `json:"cpuLimit"`
	MemoryRequest   string            `json:"memoryRequest"`
	MemoryLimit     string            `json:"memoryLimit"`
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy"`
	Env             []corev1.EnvVar   `json:"env"`
	OperatorEnv     []corev1.EnvVar   `json:"operatorEnv"`
	DummyKubeconfig []byte            `json:"dummyKubeconfig"`
	ProxyCA         []byte            `json:"proxyCA"`
}

func overlayHash(cfg session.SandboxConfig, dummy, ca []byte) (string, error) {
	raw, err := json.Marshal(overlayFingerprint{
		Image:           cfg.Image,
		CPURequest:      cfg.CPURequest,
		CPULimit:        cfg.CPULimit,
		MemoryRequest:   cfg.MemoryRequest,
		MemoryLimit:     cfg.MemoryLimit,
		ImagePullPolicy: cfg.ImagePullPolicy,
		Env:             cfg.Env,
		OperatorEnv:     session.OperatorEnv(cfg),
		DummyKubeconfig: dummy,
		ProxyCA:         ca,
	})
	if err != nil {
		return "", fmt.Errorf("hash sandbox overlay: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func quantityOr(list corev1.ResourceList, name corev1.ResourceName, fallback string) string {
	if q, ok := list[name]; ok && !q.IsZero() {
		return q.String()
	}
	return fallback
}

func replicasOrDefault(replicas int32) int32 {
	if replicas < 1 {
		return 1
	}
	return replicas
}

func idleTimeoutOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultIdleTimeout
	}
	return d
}
