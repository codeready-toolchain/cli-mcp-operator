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
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
)

func clientAuthScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	require.NoError(t, climcpv1alpha1.AddToScheme(scheme))
	return scheme
}

func testInstance(name, namespace string) *climcpv1alpha1.CliMcpInstance {
	return &climcpv1alpha1.CliMcpInstance{
		TypeMeta: metav1.TypeMeta{
			APIVersion: climcpv1alpha1.GroupVersion.String(),
			Kind:       "CliMcpInstance",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name + "-uid"),
		},
	}
}

func TestKRPConfigYAML(t *testing.T) {
	t.Parallel()
	inst := &climcpv1alpha1.CliMcpInstance{}
	inst.Name = "oc"
	inst.Namespace = "cli-mcp"
	raw, err := krpConfigYAML(inst)
	require.NoError(t, err)

	var cfg krpConfig
	require.NoError(t, yaml.Unmarshal([]byte(raw), &cfg))
	assert.Equal(t, climcpv1alpha1.GroupVersion.Group, cfg.Authorization.ResourceAttributes.APIGroup)
	assert.Equal(t, climcpv1alpha1.GroupVersion.Version, cfg.Authorization.ResourceAttributes.APIVersion)
	assert.Equal(t, "climcpinstances", cfg.Authorization.ResourceAttributes.Resource)
	assert.Equal(t, "mcp", cfg.Authorization.ResourceAttributes.Subresource)
	assert.Equal(t, "cli-mcp", cfg.Authorization.ResourceAttributes.Namespace)
	assert.Equal(t, "oc", cfg.Authorization.ResourceAttributes.Name)

	aws := &climcpv1alpha1.CliMcpInstance{}
	aws.Name = "aws"
	aws.Namespace = "cli-mcp"
	rawAWS, err := krpConfigYAML(aws)
	require.NoError(t, err)
	var cfgAWS krpConfig
	require.NoError(t, yaml.Unmarshal([]byte(rawAWS), &cfgAWS))
	assert.Equal(t, "aws", cfgAWS.Authorization.ResourceAttributes.Name)
	assert.NotEqual(t, raw, rawAWS)
}

func TestApplyClientAuthChildren(t *testing.T) {
	t.Parallel()
	scheme := clientAuthScheme(t)
	inst := testInstance("oc", "ns")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst.DeepCopy()).Build()
	r := &CliMcpInstanceReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.applyClientSA(t.Context(), inst))
	require.NoError(t, r.applyClientRole(t.Context(), inst))
	require.NoError(t, r.applyClientRoleBinding(t.Context(), inst))
	cm, err := r.applyKRPConfigMap(t.Context(), inst)
	require.NoError(t, err)

	sa := &corev1.ServiceAccount{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: clientSAName("oc"), Namespace: "ns"}, sa))
	assert.Equal(t, instanceLabels("oc"), sa.Labels)
	require.Len(t, sa.OwnerReferences, 1)
	assert.Equal(t, "oc", sa.OwnerReferences[0].Name)
	assert.Equal(t, "CliMcpInstance", sa.OwnerReferences[0].Kind)

	role := &rbacv1.Role{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: clientSAName("oc"), Namespace: "ns"}, role))
	assert.Equal(t, clientRoleRules("oc"), role.Rules)

	rb := &rbacv1.RoleBinding{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: clientSAName("oc"), Namespace: "ns"}, rb))
	assert.Equal(t, clientSAName("oc"), rb.RoleRef.Name)
	assert.Equal(t, "Role", rb.RoleRef.Kind)
	require.Len(t, rb.Subjects, 1)
	assert.Equal(t, rbacv1.ServiceAccountKind, rb.Subjects[0].Kind)
	assert.Equal(t, clientSAName("oc"), rb.Subjects[0].Name)
	assert.Equal(t, "ns", rb.Subjects[0].Namespace)

	wantYAML, err := krpConfigYAML(inst)
	require.NoError(t, err)
	assert.Equal(t, wantYAML, cm.Data[krpConfigKey])
	gotCM := &corev1.ConfigMap{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: krpConfigMapName("oc"), Namespace: "ns"}, gotCM))
	assert.Equal(t, wantYAML, gotCM.Data[krpConfigKey])
	require.Len(t, gotCM.OwnerReferences, 1)
	assert.Equal(t, "oc", gotCM.OwnerReferences[0].Name)

	role.Rules[0].ResourceNames = []string{"wrong"}
	require.NoError(t, c.Update(t.Context(), role))
	require.NoError(t, r.applyClientRole(t.Context(), inst))
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: clientSAName("oc"), Namespace: "ns"}, role))
	assert.Equal(t, clientRoleRules("oc"), role.Rules)

	gotCM.Data[krpConfigKey] = "tampered"
	require.NoError(t, c.Update(t.Context(), gotCM))
	restored, err := r.applyKRPConfigMap(t.Context(), inst)
	require.NoError(t, err)
	assert.Equal(t, wantYAML, restored.Data[krpConfigKey])
}

func TestMissingChildrenIncludesClientAuth(t *testing.T) {
	t.Parallel()
	scheme := clientAuthScheme(t)
	inst := testInstance("oc", "ns")
	objs := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: childName("oc"), Namespace: "ns"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sandboxSAName("oc"), Namespace: "ns"}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: childName("oc"), Namespace: "ns"}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: clientSAName("oc"), Namespace: "ns"}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: childName("oc"), Namespace: "ns"}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clientSAName("oc"), Namespace: "ns"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: childName("oc"), Namespace: "ns"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: sandboxSAName("oc"), Namespace: "ns"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: childName("oc"), Namespace: "ns"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hmacSecretName("oc"), Namespace: "ns"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &CliMcpInstanceReconciler{Client: c}
	missing, msg := r.missingChildren(t.Context(), inst)
	require.True(t, missing)
	assert.Equal(t, "missing children: "+clientSAName("oc")+", "+krpConfigMapName("oc"), msg)
}

func TestAuthDelegatorNotReady(t *testing.T) {
	t.Parallel()
	scheme := clientAuthScheme(t)
	inst := testInstance("oc", "ns")
	deleting := inst.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{finalizerName}

	tests := []struct {
		name     string
		inst     *climcpv1alpha1.CliMcpInstance
		objs     []client.Object
		notReady bool
		msg      string
	}{
		{
			name:     "missing CRB",
			inst:     inst,
			notReady: true,
			msg:      "missing ClusterRoleBinding " + authDelegatorCRBName,
		},
		{
			name: "foreign roleRef",
			inst: inst,
			objs: []client.Object{&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "ClusterRole",
					Name:     "not-auth-delegator",
				},
			}},
			notReady: true,
			msg:      errAuthDelegatorRoleRef.Error() + ": not-auth-delegator",
		},
		{
			name: "missing this instance subject",
			inst: inst,
			objs: []client.Object{&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
				RoleRef:    authDelegatorRoleRef(),
				Subjects:   []rbacv1.Subject{mcpSASubject("aws", "ns")},
			}},
			notReady: true,
			msg:      authDelegatorCRBName + " subjects missing MCP SA ns/" + childName("oc"),
		},
		{
			name: "deleting instance skips subject check",
			inst: deleting,
			objs: []client.Object{&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
				RoleRef:    authDelegatorRoleRef(),
			}},
			notReady: false,
		},
		{
			name: "subject present",
			inst: inst,
			objs: []client.Object{&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
				RoleRef:    authDelegatorRoleRef(),
				Subjects:   []rbacv1.Subject{mcpSASubject("oc", "ns")},
			}},
			notReady: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objs...).Build()
			r := &CliMcpInstanceReconciler{Client: c, APIReader: c}
			notReady, msg := r.authDelegatorNotReady(t.Context(), tt.inst)
			assert.Equal(t, tt.notReady, notReady)
			assert.Equal(t, tt.msg, msg)
		})
	}
}

func TestApplyAuthDelegatorCRB(t *testing.T) {
	t.Parallel()

	scheme := clientAuthScheme(t)

	newInst := func(name, namespace string, deleting bool) *climcpv1alpha1.CliMcpInstance {
		inst := testInstance(name, namespace)
		if deleting {
			now := metav1.Now()
			inst.DeletionTimestamp = &now
			inst.Finalizers = []string{finalizerName}
		}
		return inst
	}

	getCRB := func(t *testing.T, c client.Client) *rbacv1.ClusterRoleBinding {
		t.Helper()
		crb := &rbacv1.ClusterRoleBinding{}
		require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: authDelegatorCRBName}, crb))
		return crb
	}

	t.Run("creates CRB with live MCP SAs", func(t *testing.T) {
		t.Parallel()
		oc := newInst("oc", "ns-a", false)
		aws := newInst("aws", "ns-a", false)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc, aws).Build()
		r := &CliMcpInstanceReconciler{Client: c, APIReader: c, Scheme: scheme}
		require.NoError(t, r.applyAuthDelegatorCRB(t.Context()))

		crb := getCRB(t, c)
		assert.Equal(t, authDelegatorRoleRef(), crb.RoleRef)
		assert.Empty(t, crb.OwnerReferences)
		assert.Equal(t, []rbacv1.Subject{
			mcpSASubject("aws", "ns-a"),
			mcpSASubject("oc", "ns-a"),
		}, crb.Subjects)
	})

	t.Run("replaces subjects and skips deleting instances", func(t *testing.T) {
		t.Parallel()
		oc := newInst("oc", "ns-a", false)
		aws := newInst("aws", "ns-a", true)
		existing := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
			RoleRef:    authDelegatorRoleRef(),
			Subjects: []rbacv1.Subject{
				mcpSASubject("stale", "other"),
				mcpSASubject("aws", "ns-a"),
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc, aws, existing).Build()
		r := &CliMcpInstanceReconciler{Client: c, APIReader: c, Scheme: scheme}
		require.NoError(t, r.applyAuthDelegatorCRB(t.Context()))

		crb := getCRB(t, c)
		assert.Equal(t, []rbacv1.Subject{mcpSASubject("oc", "ns-a")}, crb.Subjects)
	})

	t.Run("last instance empties subjects and keeps the CRB", func(t *testing.T) {
		t.Parallel()
		last := newInst("oc", "ns-a", true)
		existing := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
			RoleRef:    authDelegatorRoleRef(),
			Subjects:   []rbacv1.Subject{mcpSASubject("oc", "ns-a")},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(last, existing).Build()
		r := &CliMcpInstanceReconciler{Client: c, APIReader: c, Scheme: scheme}
		require.NoError(t, r.applyAuthDelegatorCRB(t.Context()))

		crb := getCRB(t, c)
		assert.Empty(t, crb.Subjects)
	})

	t.Run("foreign roleRef is not overwritten", func(t *testing.T) {
		t.Parallel()
		oc := newInst("oc", "ns-a", false)
		existing := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     "not-auth-delegator",
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc, existing).Build()
		r := &CliMcpInstanceReconciler{Client: c, APIReader: c, Scheme: scheme}
		err := r.applyAuthDelegatorCRB(t.Context())
		require.Error(t, err)
		assert.True(t, errors.Is(err, errAuthDelegatorRoleRef))

		crb := getCRB(t, c)
		assert.Equal(t, "not-auth-delegator", crb.RoleRef.Name)
		assert.Empty(t, crb.Subjects)
	})

	t.Run("sorts subjects by namespace then name", func(t *testing.T) {
		t.Parallel()
		oc := newInst("oc", "ns-b", false)
		aws := newInst("aws", "ns-a", false)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc, aws).Build()
		r := &CliMcpInstanceReconciler{Client: c, APIReader: c, Scheme: scheme}
		require.NoError(t, r.applyAuthDelegatorCRB(t.Context()))
		assert.Equal(t, []rbacv1.Subject{
			mcpSASubject("aws", "ns-a"),
			mcpSASubject("oc", "ns-b"),
		}, getCRB(t, c).Subjects)
	})

	t.Run("does not update when subjects already match", func(t *testing.T) {
		t.Parallel()
		oc := newInst("oc", "ns-a", false)
		existing := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
			RoleRef:    authDelegatorRoleRef(),
			Subjects:   []rbacv1.Subject{mcpSASubject("oc", "ns-a")},
		}
		inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc, existing).Build()
		c := interceptor.NewClient(inner, interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				t.Fatal("must not update ClusterRoleBinding when subjects already match")
				return fmt.Errorf("unexpected update")
			},
		})
		r := &CliMcpInstanceReconciler{Client: c, APIReader: inner, Scheme: scheme}
		require.NoError(t, r.applyAuthDelegatorCRB(t.Context()))
	})

	t.Run("conflict on update is returned", func(t *testing.T) {
		t.Parallel()
		oc := newInst("oc", "ns-a", false)
		existing := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: authDelegatorCRBName},
			RoleRef:    authDelegatorRoleRef(),
			Subjects:   []rbacv1.Subject{mcpSASubject("stale", "other")},
		}
		inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc, existing).Build()
		c := interceptor.NewClient(inner, interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return apierrors.NewConflict(
					rbacv1.Resource("clusterrolebindings"), authDelegatorCRBName, fmt.Errorf("object was modified"))
			},
		})
		r := &CliMcpInstanceReconciler{Client: c, APIReader: inner, Scheme: scheme}
		err := r.applyAuthDelegatorCRB(t.Context())
		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err))
	})
}
