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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func generateHMACKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate HMAC key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func secretKeyNonEmpty(secret *corev1.Secret, key string) bool {
	if secret == nil {
		return false
	}
	return strings.TrimSpace(string(secret.Data[key])) != ""
}

// ensureHMAC generate-once creates cli-mcp-<name>-hmac if missing. It never
// overwrites an existing Secret (including empty/wrong key — that stays
// SecretKeysInvalid).
func (r *CliMcpInstanceReconciler) ensureHMAC(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (*corev1.Secret, error) {
	nn := types.NamespacedName{Namespace: inst.Namespace, Name: hmacSecretName(inst.Name)}
	secret := &corev1.Secret{}
	err := r.Get(ctx, nn, secret)
	if err == nil {
		if metaErr := r.adoptHMACMetadata(ctx, inst, secret); metaErr != nil {
			return nil, metaErr
		}
		return secret, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get HMAC secret: %w", err)
	}

	key, err := generateHMACKey()
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
		StringData: map[string]string{
			hmacSecretKey: key,
		},
	}
	if err := controllerutil.SetControllerReference(inst, secret, r.Scheme); err != nil {
		return nil, fmt.Errorf("HMAC ownerRef: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("create HMAC secret: %w", err)
	}
	if err := r.Get(ctx, nn, secret); err != nil {
		return nil, fmt.Errorf("get HMAC secret after create: %w", err)
	}
	return secret, nil
}

func (r *CliMcpInstanceReconciler) adoptHMACMetadata(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance, secret *corev1.Secret) error {
	before := secret.DeepCopy()
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[session.LabelInstance] = inst.Name
	if err := controllerutil.SetControllerReference(inst, secret, r.Scheme); err != nil {
		return fmt.Errorf("HMAC ownerRef: %w", err)
	}
	if equality.Semantic.DeepEqual(before.Labels, secret.Labels) &&
		equality.Semantic.DeepEqual(before.OwnerReferences, secret.OwnerReferences) {
		return nil
	}
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("update HMAC metadata: %w", err)
	}
	return nil
}
