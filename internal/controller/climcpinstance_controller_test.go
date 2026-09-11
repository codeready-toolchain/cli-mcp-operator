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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/session"
)

var _ = Describe("CliMcpInstance Controller", func() {
	var (
		ctx        context.Context
		ns         *corev1.Namespace
		reconciler *CliMcpInstanceReconciler
		nn         types.NamespacedName
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			GenerateName: "climcp-test-",
		}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nn = types.NamespacedName{Name: "oc", Namespace: ns.Name}
		reconciler = &CliMcpInstanceReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			Images:      testImages(),
			OnOpenShift: false,
		}
	})

	AfterEach(func() {
		inst := &climcpv1alpha1.CliMcpInstance{}
		if err := k8sClient.Get(ctx, nn, inst); err == nil {
			inst.Finalizers = nil
			_ = k8sClient.Update(ctx, inst)
			_ = k8sClient.Delete(ctx, inst)
		}
		_ = k8sClient.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: authDelegatorCRBName(ns.Name, "oc"),
		}})
		_ = k8sClient.Delete(ctx, ns)
	})

	It("applies children, generate-once HMAC, and goes Ready when admin secrets are valid", func() {
		createAdminSecrets(ctx, ns.Name)
		Expect(k8sClient.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name:      legacyServerChildName("oc"),
			Namespace: ns.Name,
		}})).To(Succeed())
		createInstance(ctx, nn)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hmac := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hmacSecretName("oc"), Namespace: ns.Name}, hmac)).To(Succeed())
		Expect(secretKeyNonEmpty(hmac, hmacSecretKey)).To(BeTrue())
		firstKey := string(hmac.Data[hmacSecretKey])

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpServerSAName("oc"), Namespace: ns.Name}, &corev1.ServiceAccount{})).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: legacyServerChildName("oc"), Namespace: ns.Name}, &corev1.ServiceAccount{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: legacyServerChildName("oc") + "-server", Namespace: ns.Name}, &corev1.ServiceAccount{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sandboxSAName("oc"), Namespace: ns.Name}, &corev1.ServiceAccount{})).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: legacyServerChildName("oc") + "-sandbox", Namespace: ns.Name}, &corev1.ServiceAccount{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, &corev1.Service{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sandboxSAName("oc"), Namespace: ns.Name}, &networkingv1.NetworkPolicy{})).To(Succeed())

		crb := &rbacv1.ClusterRoleBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authDelegatorCRBName(ns.Name, "oc")}, crb)).To(Succeed())
		Expect(crb.RoleRef.Name).To(Equal(authDelegatorClusterRole))
		Expect(crb.Subjects).To(HaveLen(1))
		Expect(crb.Subjects[0].Name).To(Equal(mcpServerSAName("oc")))
		Expect(crb.Subjects[0].Namespace).To(Equal(ns.Name))
		Expect(crb.OwnerReferences).To(BeEmpty())

		role := &rbacv1.Role{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, role)).To(Succeed())
		var secretVerbs []string
		for _, rule := range role.Rules {
			for _, res := range rule.Resources {
				if res == "secrets" {
					secretVerbs = rule.Verbs
				}
			}
		}
		Expect(secretVerbs).To(ConsistOf("create", "delete"))

		rb := &rbacv1.RoleBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, rb)).To(Succeed())
		Expect(rb.Subjects).To(HaveLen(1))
		Expect(rb.Subjects[0].Name).To(Equal(mcpServerSAName("oc")))

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, deploy)).To(Succeed())
		Expect(deploy.Spec.Template.Spec.ServiceAccountName).To(Equal(mcpServerSAName("oc")))
		args := deploy.Spec.Template.Spec.Containers[0].Args
		Expect(args).NotTo(ContainElement("--warm-pool-size"))
		Expect(args).NotTo(ContainElement("--idle-timeout"))
		Expect(args).To(ContainElement("--instance-name"))
		Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hmacSecretName("oc"), Namespace: ns.Name}, hmac)).To(Succeed())
		Expect(string(hmac.Data[hmacSecretKey])).To(Equal(firstKey))

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			inst := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
			cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(inst.Status.WarmPoolDesired).To(Equal(int32(0)))
			g.Expect(inst.Status.WarmPoolReady).To(Equal(int32(0)))
			g.Expect(inst.Status.ResolvedSandboxImage).To(Equal(testImages().Sandbox))
		}).Should(Succeed())

		deploy = &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, deploy)).To(Succeed())
		gen := deploy.Generation
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, deploy)).To(Succeed())
		Expect(deploy.Generation).To(Equal(gen))
	})

	It("sets SecretsNotFound when kubeconfig is missing", func() {
		createTLSSecret(ctx, ns.Name)
		createInstance(ctx, nn)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(climcpv1alpha1.ReasonSecretsNotFound))
	})

	It("sets SecretKeysInvalid for empty kubeconfig and HMAC keys", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: kubeconfigSecretName("oc"), Namespace: ns.Name},
			Data:       map[string][]byte{kubeconfigDataKey: []byte("  ")},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: hmacSecretName("oc"), Namespace: ns.Name},
			Data:       map[string][]byte{hmacSecretKey: []byte("")},
		})).To(Succeed())
		createTLSSecret(ctx, ns.Name)
		createInstance(ctx, nn)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		hmac := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hmacSecretName("oc"), Namespace: ns.Name}, hmac)).To(Succeed())
		Expect(secretKeyNonEmpty(hmac, hmacSecretKey)).To(BeFalse())

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(climcpv1alpha1.ReasonSecretKeysInvalid))
	})

	It("sets SecretKeysInvalid for empty TLS keys on generic Kubernetes", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: kubeconfigSecretName("oc"), Namespace: ns.Name},
			Data:       map[string][]byte{kubeconfigDataKey: []byte("dummy")},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tlsSecretName("oc"), Namespace: ns.Name},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				tlsCertKey: []byte(""),
				tlsKeyKey:  []byte("key"),
			},
		})).To(Succeed())
		createInstance(ctx, nn)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(climcpv1alpha1.ReasonSecretKeysInvalid))
	})

	It("GCs idle assigned sessions, skips unassigned pool pods, and requeues remaining", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstanceWithPool(ctx, nn, 1)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		poolPods := listUnassignedSandbox(ctx, ns.Name)
		Expect(poolPods).To(HaveLen(1))
		poolName := poolPods[0].Name
		staleActivity := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		poolPods[0].Annotations[session.AnnotationLastActivity] = staleActivity
		poolPods[0].Annotations[session.AnnotationCreatedAt] = staleActivity
		Expect(k8sClient.Update(ctx, &poolPods[0])).To(Succeed())

		now := time.Now().UTC()
		idle := sandboxPodObject(ns.Name, "idle-pod", "idle-sess", now.Add(-time.Hour), now.Add(-time.Hour))
		Expect(k8sClient.Create(ctx, idle)).To(Succeed())
		Expect(k8sClient.Create(ctx, sessionSecret(ns.Name, "idle-sess"))).To(Succeed())

		fresh := sandboxPodObject(ns.Name, "fresh-pod", "fresh-sess", now, now)
		Expect(k8sClient.Create(ctx, fresh)).To(Succeed())

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "idle-pod", Namespace: ns.Name}, &corev1.Pod{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: session.AuthSecretName("idle-sess"), Namespace: ns.Name}, &corev1.Secret{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fresh-pod", Namespace: ns.Name}, &corev1.Pod{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName, Namespace: ns.Name}, &corev1.Pod{})).To(Succeed())
	})

	It("finalizer scales MCP to 0, waits for server pods, then deletes sandboxes", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstance(ctx, nn)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		serverPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mcp-server",
				Namespace: ns.Name,
				Labels:    serverLabels("oc"),
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "pause"}}},
		}
		Expect(k8sClient.Create(ctx, serverPod)).To(Succeed())
		sess := sandboxPodObject(ns.Name, "sess-pod", "sess-1", time.Now(), time.Now())
		Expect(k8sClient.Create(ctx, sess)).To(Succeed())
		Expect(k8sClient.Create(ctx, sessionSecret(ns.Name, "sess-1"))).To(Succeed())

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		Expect(k8sClient.Delete(ctx, inst)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, deploy)).To(Succeed())
		Expect(*deploy.Spec.Replicas).To(Equal(int32(0)))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sess-pod", Namespace: ns.Name}, &corev1.Pod{})).To(Succeed())

		Expect(k8sClient.Delete(ctx, serverPod)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "sess-pod", Namespace: ns.Name}, &corev1.Pod{}))).To(BeTrue())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, nn, inst)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: authDelegatorCRBName(ns.Name, "oc")}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: kubeconfigSecretName("oc"), Namespace: ns.Name}, &corev1.Secret{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tlsSecretName("oc"), Namespace: ns.Name}, &corev1.Secret{})).To(Succeed())
	})

	It("rejects CR names longer than 44 characters", func() {
		inst := &climcpv1alpha1.CliMcpInstance{ObjectMeta: metav1.ObjectMeta{
			Name:      strings.Repeat("a", 45),
			Namespace: ns.Name,
		}}
		err := k8sClient.Create(ctx, inst)
		Expect(err).To(HaveOccurred())
	})

	It("sets serving-cert annotation on OpenShift and does not require TLS for Ready", func() {
		reconciler.OnOpenShift = true
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: kubeconfigSecretName("oc"), Namespace: ns.Name},
			Data:       map[string][]byte{kubeconfigDataKey: []byte("dummy")},
		})).To(Succeed())
		createInstance(ctx, nn)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, svc)).To(Succeed())
		Expect(svc.Annotations[openshiftServingCertAnnotation]).To(Equal(tlsSecretName("oc")))

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			inst := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
			cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}).Should(Succeed())
	})

	It("does not gate Ready on extra spec.sandbox.env Secrets", func() {
		createAdminSecrets(ctx, ns.Name)
		inst := &climcpv1alpha1.CliMcpInstance{
			ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
			Spec: climcpv1alpha1.CliMcpInstanceSpec{
				Replicas: 1,
				Sandbox: climcpv1alpha1.SandboxSpec{
					IdleTimeout:  metav1.Duration{Duration: 30 * time.Minute},
					WarmPoolSize: 0,
					Env: []corev1.EnvVar{{
						Name: "AWS_REGION",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "aws-cli-creds"},
								Key:                  "region",
							},
						},
					}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			got := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}).Should(Succeed())
	})

	It("maintains warmPoolSize, skips claimed pods, and does not flap Ready", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstanceWithPool(ctx, nn, 2)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, deploy)).To(Succeed())
		gen := deploy.Generation

		unassigned := listUnassignedSandbox(ctx, ns.Name)
		Expect(unassigned).To(HaveLen(2))
		for i := range unassigned {
			Expect(unassigned[i].Labels).NotTo(HaveKey(session.LabelSessionID))
			Expect(unassigned[i].Annotations[sandboxOverlayAnnotation]).NotTo(BeEmpty())
			Expect(unassigned[i].OwnerReferences).NotTo(BeEmpty())
			for _, env := range unassigned[i].Spec.Containers[0].Env {
				Expect(env.Name).NotTo(Equal("SANDBOX_AUTH_TOKEN"))
			}
			markPodReady(ctx, &unassigned[i])
		}

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			inst := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
			cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(inst.Status.WarmPoolDesired).To(Equal(int32(2)))
			g.Expect(inst.Status.WarmPoolReady).To(Equal(int32(2)))
			warm := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionWarmPoolReady)
			g.Expect(warm).NotTo(BeNil())
			g.Expect(warm.Status).To(Equal(metav1.ConditionTrue))
		}).Should(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, deploy)).To(Succeed())
		Expect(deploy.Generation).To(Equal(gen))

		claimed := listUnassignedSandbox(ctx, ns.Name)[0]
		claimed.Labels[session.LabelSessionID] = "claimed-sess"
		Expect(k8sClient.Update(ctx, &claimed)).To(Succeed())

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 4*time.Minute))
		Expect(result.RequeueAfter).To(BeNumerically("<=", poolReplenishDeadline))

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(inst.Status.WarmPoolReady).To(BeNumerically("<", inst.Status.WarmPoolDesired))
		warm := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionWarmPoolReady)
		Expect(warm).NotTo(BeNil())
		Expect(warm.Status).To(Equal(metav1.ConditionFalse))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: claimed.Name, Namespace: ns.Name}, &corev1.Pod{})).To(Succeed())
		Expect(listUnassignedSandbox(ctx, ns.Name)).To(HaveLen(2))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName("oc"), Namespace: ns.Name}, deploy)).To(Succeed())
		Expect(deploy.Generation).To(Equal(gen))
	})

	It("trims surplus oldest first and leaves assigned pods on overlay hash change", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstanceWithPool(ctx, nn, 1)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		oldestName := listUnassignedSandbox(ctx, ns.Name)[0].Name

		time.Sleep(time.Second)
		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		inst.Spec.Sandbox.WarmPoolSize = 2
		Expect(k8sClient.Update(ctx, inst)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(listUnassignedSandbox(ctx, ns.Name)).To(HaveLen(2))

		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		inst.Spec.Sandbox.WarmPoolSize = 1
		Expect(k8sClient.Update(ctx, inst)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		remaining := listUnassignedSandbox(ctx, ns.Name)
		Expect(remaining).To(HaveLen(1))
		Expect(remaining[0].Name).NotTo(Equal(oldestName))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: oldestName, Namespace: ns.Name}, &corev1.Pod{}))).To(BeTrue())

		extra := sandboxPodObject(ns.Name, "extra-unassigned", "", time.Now(), time.Now())
		delete(extra.Labels, session.LabelSessionID)
		Expect(k8sClient.Create(ctx, extra)).To(Succeed())

		assigned := sandboxPodObject(ns.Name, "assigned-keep", "keep-sess", time.Now(), time.Now())
		Expect(k8sClient.Create(ctx, assigned)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "extra-unassigned", Namespace: ns.Name}, &corev1.Pod{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "assigned-keep", Namespace: ns.Name}, &corev1.Pod{})).To(Succeed())
		Expect(listUnassignedSandbox(ctx, ns.Name)).To(HaveLen(1))

		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		inst.Spec.Sandbox.Image = "example.com/cli-mcp-sandbox:other"
		Expect(k8sClient.Update(ctx, inst)).To(Succeed())

		before := listUnassignedSandbox(ctx, ns.Name)
		Expect(before).To(HaveLen(1))
		oldName := before[0].Name

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: oldName, Namespace: ns.Name}, &corev1.Pod{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "assigned-keep", Namespace: ns.Name}, &corev1.Pod{})).To(Succeed())
		replaced := listUnassignedSandbox(ctx, ns.Name)
		Expect(replaced).To(HaveLen(1))
		Expect(replaced[0].Name).NotTo(Equal(oldName))
		Expect(replaced[0].Spec.Containers[0].Image).To(Equal("example.com/cli-mcp-sandbox:other"))
	})

	It("clears Ready on overlay rebuild until the replacement pool is Ready", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstanceWithPool(ctx, nn, 1)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		unassigned := listUnassignedSandbox(ctx, ns.Name)
		Expect(unassigned).To(HaveLen(1))
		markPodReady(ctx, &unassigned[0])

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			inst := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
			cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}).Should(Succeed())

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		inst.Spec.Sandbox.Image = "example.com/cli-mcp-sandbox:rebuilt"
		Expect(k8sClient.Update(ctx, inst)).To(Succeed())

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			got := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(climcpv1alpha1.ReasonWarmPoolNotReady))
		}).Should(Succeed())

		replaced := listUnassignedSandbox(ctx, ns.Name)
		Expect(replaced).To(HaveLen(1))
		Expect(replaced[0].Spec.Containers[0].Image).To(Equal("example.com/cli-mcp-sandbox:rebuilt"))
		markPodReady(ctx, &replaced[0])

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			got := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(got.Status.WarmPoolReady).To(Equal(int32(1)))
		}).Should(Succeed())
	})

	It("replaces a Failed pool pod and clears Ready", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstanceWithPool(ctx, nn, 1)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		unassigned := listUnassignedSandbox(ctx, ns.Name)
		Expect(unassigned).To(HaveLen(1))
		markPodReady(ctx, &unassigned[0])
		oldName := unassigned[0].Name

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			inst := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
			cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}).Should(Succeed())

		failed := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: oldName, Namespace: ns.Name}, failed)).To(Succeed())
		failed.Status.Phase = corev1.PodFailed
		failed.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
		}}
		Expect(k8sClient.Status().Update(ctx, failed)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(climcpv1alpha1.ReasonWarmPoolUnhealthy))
		replaced := listUnassignedSandbox(ctx, ns.Name)
		Expect(replaced).To(HaveLen(1))
		Expect(replaced[0].Name).NotTo(Equal(oldName))
	})

	It("does not go Ready until the warm pool is full on first Ready", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstanceWithPool(ctx, nn, 2)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			inst := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
			cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(climcpv1alpha1.ReasonWarmPoolNotReady))
		}).Should(Succeed())
	})

	It("does not go Ready until the pool is full after a size increase", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstanceWithPool(ctx, nn, 1)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		unassigned := listUnassignedSandbox(ctx, ns.Name)
		Expect(unassigned).To(HaveLen(1))
		markPodReady(ctx, &unassigned[0])

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			inst := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
			cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(inst.Status.WarmPoolDesired).To(Equal(int32(1)))
		}).Should(Succeed())

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		inst.Spec.Sandbox.WarmPoolSize = 2
		Expect(k8sClient.Update(ctx, inst)).To(Succeed())

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			got := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
			g.Expect(got.Status.WarmPoolDesired).To(Equal(int32(2)))
			cond := meta.FindStatusCondition(got.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(climcpv1alpha1.ReasonWarmPoolNotReady))
		}).Should(Succeed())
		Expect(listUnassignedSandbox(ctx, ns.Name)).To(HaveLen(2))

		for _, pod := range listUnassignedSandbox(ctx, ns.Name) {
			p := pod
			markPodReady(ctx, &p)
		}

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			got := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(got.Status.WarmPoolReady).To(Equal(int32(2)))
		}).Should(Succeed())
	})

	It("replaces an ImagePullBackOff pool pod and clears Ready", func() {
		createAdminSecrets(ctx, ns.Name)
		createInstanceWithPool(ctx, nn, 1)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		unassigned := listUnassignedSandbox(ctx, ns.Name)
		Expect(unassigned).To(HaveLen(1))
		markPodReady(ctx, &unassigned[0])
		oldName := unassigned[0].Name

		Eventually(func(g Gomega) {
			markDeploymentAvailable(ctx, ns.Name, childName("oc"))
			_, recErr := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(recErr).NotTo(HaveOccurred())
			inst := &climcpv1alpha1.CliMcpInstance{}
			g.Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
			cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}).Should(Succeed())

		backoff := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: oldName, Namespace: ns.Name}, backoff)).To(Succeed())
		backoff.Status.Phase = corev1.PodPending
		backoff.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
		}}
		backoff.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "sandbox",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: session.WaitingImagePullBackOff,
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, backoff)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		inst := &climcpv1alpha1.CliMcpInstance{}
		Expect(k8sClient.Get(ctx, nn, inst)).To(Succeed())
		cond := meta.FindStatusCondition(inst.Status.Conditions, climcpv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(climcpv1alpha1.ReasonWarmPoolUnhealthy))
		replaced := listUnassignedSandbox(ctx, ns.Name)
		Expect(replaced).To(HaveLen(1))
		Expect(replaced[0].Name).NotTo(Equal(oldName))
	})
})

func testImages() Images {
	return Images{
		Server:        "example.com/cli-mcp-server:test",
		Sandbox:       "example.com/cli-mcp-sandbox:test",
		KubeRBACProxy: "example.com/kube-rbac-proxy:test",
	}
}

func createInstance(ctx context.Context, nn types.NamespacedName) {
	GinkgoHelper()
	createInstanceWithPool(ctx, nn, 0)
}

func createInstanceWithPool(ctx context.Context, nn types.NamespacedName, size int32) {
	GinkgoHelper()
	inst := &climcpv1alpha1.CliMcpInstance{
		ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
		Spec: climcpv1alpha1.CliMcpInstanceSpec{
			Replicas: 1,
			Sandbox: climcpv1alpha1.SandboxSpec{
				IdleTimeout:  metav1.Duration{Duration: 30 * time.Minute},
				WarmPoolSize: size,
			},
		},
	}
	Expect(k8sClient.Create(ctx, inst)).To(Succeed())
}

func listUnassignedSandbox(ctx context.Context, namespace string) []corev1.Pod {
	GinkgoHelper()
	var pods corev1.PodList
	Expect(k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(sandboxLabels("oc")))).To(Succeed())
	var out []corev1.Pod
	for i := range pods.Items {
		if _, assigned := pods.Items[i].Labels[session.LabelSessionID]; assigned {
			continue
		}
		if pods.Items[i].DeletionTimestamp != nil {
			continue
		}
		out = append(out, pods.Items[i])
	}
	return out
}

func markPodReady(ctx context.Context, pod *corev1.Pod) {
	GinkgoHelper()
	fresh := &corev1.Pod{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, fresh)).To(Succeed())
	fresh.Status.Phase = corev1.PodRunning
	fresh.Status.PodIP = "10.0.0.10"
	fresh.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}

func createAdminSecrets(ctx context.Context, namespace string) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: kubeconfigSecretName("oc"), Namespace: namespace},
		Data:       map[string][]byte{kubeconfigDataKey: []byte("apiVersion: v1\nkind: Config\n")},
	})).To(Succeed())
	createTLSSecret(ctx, namespace)
}

func createTLSSecret(ctx context.Context, namespace string) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tlsSecretName("oc"), Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			tlsCertKey: []byte("cert"),
			tlsKeyKey:  []byte("key"),
		},
	})).To(Succeed())
}

func markDeploymentAvailable(ctx context.Context, namespace, name string) {
	GinkgoHelper()
	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, deploy)).To(Succeed())
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = desired
	deploy.Status.ReadyReplicas = desired
	deploy.Status.AvailableReplicas = desired
	deploy.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:               appsv1.DeploymentAvailable,
		Status:             corev1.ConditionTrue,
		LastUpdateTime:     metav1.Now(),
		LastTransitionTime: metav1.Now(),
		Reason:             "MinimumReplicasAvailable",
		Message:            "test",
	}}
	Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())
}

func sandboxPodObject(namespace, name, sessionID string, created, activity time.Time) *corev1.Pod {
	labels := sandboxLabels("oc")
	if sessionID != "" {
		labels[session.LabelSessionID] = sessionID
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
			Annotations: map[string]string{
				session.AnnotationCreatedAt:    created.UTC().Format(time.RFC3339),
				session.AnnotationLastActivity: activity.UTC().Format(time.RFC3339),
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox", Image: "pause"}}},
	}
}

func sessionSecret(namespace, sessionID string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      session.AuthSecretName(sessionID),
			Namespace: namespace,
			Labels:    map[string]string{session.LabelInstance: "oc", session.LabelComponent: session.ComponentSandbox, session.LabelSessionID: sessionID},
		},
		Data: map[string][]byte{"token": []byte("x")},
	}
}
