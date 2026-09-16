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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"
)

var errAuthDelegatorRoleRef = errors.New("ClusterRoleBinding " + authDelegatorCRBName + " has a different roleRef")

type krpConfig struct {
	Authorization krpAuthorization `json:"authorization"`
}

type krpAuthorization struct {
	ResourceAttributes krpResourceAttributes `json:"resourceAttributes"`
}

type krpResourceAttributes struct {
	APIGroup    string `json:"apiGroup"`
	APIVersion  string `json:"apiVersion"`
	Resource    string `json:"resource"`
	Subresource string `json:"subresource"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
}

func (r *CliMcpInstanceReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *CliMcpInstanceReconciler) applyClientSA(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      clientSAName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = instanceLabels(inst.Name)
		return controllerutil.SetControllerReference(inst, sa, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply client SA: %w", err)
	}
	return nil
}

func clientRoleRules(instance string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups:     []string{climcpv1alpha1.GroupVersion.Group},
		Resources:     []string{"climcpinstances/mcp"},
		ResourceNames: []string{instance},
		Verbs:         []string{"get", "create", "delete"},
	}}
}

func (r *CliMcpInstanceReconciler) applyClientRole(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name:      clientSAName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = instanceLabels(inst.Name)
		role.Rules = clientRoleRules(inst.Name)
		return controllerutil.SetControllerReference(inst, role, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply client Role: %w", err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) applyClientRoleBinding(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) error {
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      clientSAName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Labels = instanceLabels(inst.Name)
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     clientSAName(inst.Name),
		}
		rb.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      clientSAName(inst.Name),
			Namespace: inst.Namespace,
		}}
		return controllerutil.SetControllerReference(inst, rb, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply client RoleBinding: %w", err)
	}
	return nil
}

func krpConfigYAML(inst *climcpv1alpha1.CliMcpInstance) (string, error) {
	cfg := krpConfig{
		Authorization: krpAuthorization{
			ResourceAttributes: krpResourceAttributes{
				APIGroup:    climcpv1alpha1.GroupVersion.Group,
				APIVersion:  climcpv1alpha1.GroupVersion.Version,
				Resource:    "climcpinstances",
				Subresource: "mcp",
				Namespace:   inst.Namespace,
				Name:        inst.Name,
			},
		},
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal kube-rbac-proxy config: %w", err)
	}
	return string(raw), nil
}

func (r *CliMcpInstanceReconciler) applyKRPConfigMap(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (*corev1.ConfigMap, error) {
	data, err := krpConfigYAML(inst)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      krpConfigMapName(inst.Name),
		Namespace: inst.Namespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = instanceLabels(inst.Name)
		cm.Data = map[string]string{krpConfigKey: data}
		return controllerutil.SetControllerReference(inst, cm, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("apply kube-rbac-proxy ConfigMap: %w", err)
	}
	return cm, nil
}

func authDelegatorRoleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "ClusterRole",
		Name:     authDelegatorClusterRole,
	}
}

func mcpSASubject(name, namespace string) rbacv1.Subject {
	return rbacv1.Subject{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      childName(name),
		Namespace: namespace,
	}
}

func (r *CliMcpInstanceReconciler) desiredAuthDelegatorSubjects(ctx context.Context) ([]rbacv1.Subject, error) {
	var list climcpv1alpha1.CliMcpInstanceList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	subjects := make([]rbacv1.Subject, 0, len(list.Items))
	for i := range list.Items {
		inst := &list.Items[i]
		if inst.DeletionTimestamp != nil {
			continue
		}
		subjects = append(subjects, mcpSASubject(inst.Name, inst.Namespace))
	}
	slices.SortFunc(subjects, func(a, b rbacv1.Subject) int {
		if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return subjects, nil
}

func (r *CliMcpInstanceReconciler) applyAuthDelegatorCRB(ctx context.Context) error {
	subjects, err := r.desiredAuthDelegatorSubjects(ctx)
	if err != nil {
		return fmt.Errorf("list CliMcpInstance for auth-delegator: %w", err)
	}

	crb := &rbacv1.ClusterRoleBinding{}
	err = r.apiReader().Get(ctx, types.NamespacedName{Name: authDelegatorCRBName}, crb)
	if apierrors.IsNotFound(err) {
		crb = &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
			RoleRef:    authDelegatorRoleRef(),
			Subjects:   subjects,
		}
		if err = r.Create(ctx, crb); err != nil {
			return fmt.Errorf("create %s: %w", authDelegatorCRBName, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s: %w", authDelegatorCRBName, err)
	}
	if crb.RoleRef != authDelegatorRoleRef() {
		return fmt.Errorf("%w: got %s/%s", errAuthDelegatorRoleRef, crb.RoleRef.Kind, crb.RoleRef.Name)
	}
	if equality.Semantic.DeepEqual(crb.Subjects, subjects) {
		return nil
	}
	crb.Subjects = subjects
	if err = r.Update(ctx, crb); err != nil {
		return fmt.Errorf("update %s: %w", authDelegatorCRBName, err)
	}
	return nil
}

func (r *CliMcpInstanceReconciler) authDelegatorNotReady(ctx context.Context, inst *climcpv1alpha1.CliMcpInstance) (bool, string) {
	crb := &rbacv1.ClusterRoleBinding{}
	err := r.apiReader().Get(ctx, types.NamespacedName{Name: authDelegatorCRBName}, crb)
	if apierrors.IsNotFound(err) {
		return true, "missing ClusterRoleBinding " + authDelegatorCRBName
	}
	if err != nil {
		return true, err.Error()
	}
	if crb.RoleRef != authDelegatorRoleRef() {
		return true, fmt.Sprintf("%s: %s", errAuthDelegatorRoleRef.Error(), crb.RoleRef.Name)
	}
	if inst.DeletionTimestamp != nil {
		return false, ""
	}
	want := mcpSASubject(inst.Name, inst.Namespace)
	if !slices.ContainsFunc(crb.Subjects, func(s rbacv1.Subject) bool {
		return s.Kind == want.Kind && s.Name == want.Name && s.Namespace == want.Namespace
	}) {
		return true, fmt.Sprintf("%s subjects missing MCP SA %s/%s", authDelegatorCRBName, inst.Namespace, want.Name)
	}
	return false, ""
}
