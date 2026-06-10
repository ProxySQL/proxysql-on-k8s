/*
Copyright 2026 ProxySQL.

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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
)

var _ = Describe("ProxySQLCluster Controller", func() {
	const ns = "default"

	var (
		cluster    *proxysqlv1alpha1.ProxySQLCluster
		reconciler *ProxySQLClusterReconciler
		req        reconcile.Request
	)

	BeforeEach(func() {
		reconciler = &ProxySQLClusterReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	// reconcileAndExpectSuccess runs Reconcile once and fails the spec if it errors.
	reconcileAndExpectSuccess := func(name string) {
		ctx := context.Background()
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
	}

	// makeCluster creates a ProxySQLCluster with optional mutations and ensures
	// it's deleted in an AfterEach so tests are isolated.
	makeCluster := func(name string, mut ...func(*proxysqlv1alpha1.ProxySQLCluster)) *proxysqlv1alpha1.ProxySQLCluster {
		ctx := context.Background()
		c := &proxysqlv1alpha1.ProxySQLCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		}
		for _, m := range mut {
			m(c)
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			// Delete the cluster and every operator-owned resource. The garbage
			// collector that runs in production isn't present in envtest, so
			// owned objects are cleaned by name here.
			_ = k8sClient.Delete(ctx, c)
			for _, obj := range []client.Object{
				&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name + "-headless", Namespace: ns}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
				&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
			} {
				_ = k8sClient.Delete(ctx, obj)
			}
		})
		// Re-Get so we have the resourceVersion populated for subsequent updates.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, c)).To(Succeed())
		return c
	}

	When("a ProxySQLCluster is created with defaults", func() {
		const name = "pxc-defaults"

		BeforeEach(func() {
			cluster = makeCluster(name)
			reconcileAndExpectSuccess(name)
		})

		It("creates the admin Secret with all three password keys", func() {
			ctx := context.Background()
			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &sec)).To(Succeed())
			for _, k := range []string{builders.SecretKeyAdminPassword, builders.SecretKeyRadminPassword, builders.SecretKeyMonitorPassword} {
				Expect(sec.Data[k]).NotTo(BeEmpty(), "secret should have key %s populated", k)
			}
			Expect(sec.Data[builders.SecretKeyAdminPassword]).To(HaveLen(32), "minted passwords should be 32-char hex")
			Expect(isOwnedBy(&sec, cluster)).To(BeTrue(), "Secret should be controller-owned")
		})

		It("creates a ConfigMap containing the bootstrap proxysql.cnf", func() {
			ctx := context.Background()
			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("proxysql.cnf"))
			Expect(cm.Data["proxysql.cnf"]).To(ContainSubstring("admin_credentials="))
			Expect(cm.Data["proxysql.cnf"]).To(ContainSubstring("proxysql_servers="), "replicas=3 should populate cluster sync")
			Expect(isOwnedBy(&cm, cluster)).To(BeTrue())
		})

		It("creates both the regular and headless Services", func() {
			ctx := context.Background()
			var svc, headless corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &svc)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-headless", Namespace: ns}, &headless)).To(Succeed())

			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(svc.Spec.ClusterIP).NotTo(Equal(corev1.ClusterIPNone))
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("proxysql.com/cluster", name))

			Expect(headless.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
			Expect(headless.Spec.PublishNotReadyAddresses).To(BeTrue())

			portNames := func(s corev1.Service) []string {
				out := make([]string, 0, len(s.Spec.Ports))
				for _, p := range s.Spec.Ports {
					out = append(out, p.Name)
				}
				return out
			}
			Expect(portNames(svc)).To(ContainElements("mysql", "admin", "metrics"))
			Expect(portNames(headless)).To(ContainElements("mysql", "admin"))
			Expect(portNames(headless)).NotTo(ContainElement("metrics"), "headless service should not expose metrics")

			Expect(isOwnedBy(&svc, cluster)).To(BeTrue())
			Expect(isOwnedBy(&headless, cluster)).To(BeTrue())
		})

		It("creates a StatefulSet with the cnf-checksum annotation", func() {
			ctx := context.Background()
			var ss appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &ss)).To(Succeed())
			Expect(*ss.Spec.Replicas).To(Equal(int32(3)))
			Expect(ss.Spec.ServiceName).To(Equal(name + "-headless"))
			Expect(ss.Spec.Template.Annotations).To(HaveKey("proxysql.com/cnf-checksum"))
			Expect(ss.Spec.Template.Annotations["proxysql.com/cnf-checksum"]).NotTo(BeEmpty())
			Expect(ss.Spec.VolumeClaimTemplates).To(HaveLen(1), "default persistence should produce a PVC template")
			Expect(ss.Spec.VolumeClaimTemplates[0].Name).To(Equal("data"))

			c := ss.Spec.Template.Spec.Containers[0]
			Expect(c.Image).To(Equal(builders.DefaultProxySQLImage + ":" + builders.DefaultProxySQLTag))
			Expect(c.SecurityContext.ReadOnlyRootFilesystem).NotTo(BeNil())
			Expect(*c.SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())

			Expect(isOwnedBy(&ss, cluster)).To(BeTrue())
		})

		It("creates a PodDisruptionBudget keeping minAvailable=replicas-1", func() {
			ctx := context.Background()
			var pdb policyv1.PodDisruptionBudget
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pdb)).To(Succeed())
			Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
			Expect(pdb.Spec.MinAvailable.IntValue()).To(Equal(2)) // 3 - 1
			Expect(isOwnedBy(&pdb, cluster)).To(BeTrue())
		})

		It("populates .status after reconcile", func() {
			ctx := context.Background()
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			Expect(cluster.Status.ObservedGeneration).To(Equal(cluster.Generation))
			Expect(cluster.Status.Replicas).To(Equal(int32(3)))
			Expect(cluster.Status.AdminSecretName).To(Equal(name))
		})

		It("reports phase and endpoints", func() {
			ctx := context.Background()
			var got proxysqlv1alpha1.ProxySQLCluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
			// envtest has no kubelet: the StatefulSet exists but no pod ever
			// becomes ready, so the projected phase is Creating.
			Expect(got.Status.Phase).To(Equal(proxysqlv1alpha1.PhaseCreating))
			Expect(got.Status.UpdatedReplicas).To(Equal(int32(0)))
			Expect(got.Status.Endpoints).NotTo(BeNil())
			Expect(got.Status.Endpoints.Admin).To(Equal(name + "." + ns + ".svc:6032"))
			Expect(got.Status.Endpoints.MySQL).To(Equal(name + "." + ns + ".svc:6033"))
			Expect(got.Status.Endpoints.Metrics).To(Equal(name + "." + ns + ".svc:6070"))
			Expect(got.Status.Endpoints.PostgreSQL).To(BeEmpty(), "pgsql disabled by default")
			Expect(got.Status.Endpoints.Web).To(BeEmpty(), "web disabled by default")
		})

		It("is idempotent — a second reconcile keeps the same generation on owned objects", func() {
			ctx := context.Background()
			var first, second appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &first)).To(Succeed())

			reconcileAndExpectSuccess(name)

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &second)).To(Succeed())
			Expect(second.Generation).To(Equal(first.Generation), "no spec changes should not bump StatefulSet generation")
		})
	})

	When("the user changes replicas", func() {
		const name = "pxc-scale"

		It("propagates the new replica count to the StatefulSet", func() {
			ctx := context.Background()
			cluster = makeCluster(name)
			reconcileAndExpectSuccess(name)

			// Scale to 5.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			five := int32(5)
			cluster.Spec.Replicas = &five
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			reconcileAndExpectSuccess(name)

			var ss appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &ss)).To(Succeed())
			Expect(*ss.Spec.Replicas).To(Equal(int32(5)))
		})
	})

	When("the user provides a pre-existing auth Secret", func() {
		const name = "pxc-external-secret"
		const secretName = "byo-admin-creds"

		It("uses the external Secret without minting a new one", func() {
			ctx := context.Background()
			// Pre-create the auth secret.
			externalSec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
				Data: map[string][]byte{
					builders.SecretKeyAdminPassword:   []byte("admin-from-user"),
					builders.SecretKeyRadminPassword:  []byte("radmin-from-user"),
					builders.SecretKeyMonitorPassword: []byte("monitor-from-user"),
				},
			}
			Expect(k8sClient.Create(ctx, externalSec)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, externalSec) })

			cluster = makeCluster(name, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.Auth.SecretName = secretName
			})
			reconcileAndExpectSuccess(name)

			// Status should reference the BYO secret.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			Expect(cluster.Status.AdminSecretName).To(Equal(secretName))

			// The bootstrap cnf should embed the user-supplied admin password
			// (proves the operator read from the BYO secret, not a minted one).
			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &cm)).To(Succeed())
			Expect(cm.Data["proxysql.cnf"]).To(ContainSubstring("admin:admin-from-user"))

			// The operator must NOT have created a Secret named after the cluster.
			var clusterNamedSec corev1.Secret
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &clusterNamedSec)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "operator should not mint a secret when SecretName is set")
		})
	})

	When("replicas=1", func() {
		const name = "pxc-single"

		It("does not create a PodDisruptionBudget", func() {
			ctx := context.Background()
			one := int32(1)
			cluster = makeCluster(name, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.Replicas = &one
			})
			reconcileAndExpectSuccess(name)

			var pdb policyv1.PodDisruptionBudget
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pdb)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "PDB should be omitted when replicas <= 1")
		})
	})
})

// TestDerivePhase exercises the coarse phase projection directly (plain table
// test; no envtest objects involved).
func TestDerivePhase(t *testing.T) {
	now := metav1.Now()
	created := func(ready, updated int32, current, update string) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: now},
			Status: appsv1.StatefulSetStatus{
				ReadyReplicas:   ready,
				UpdatedReplicas: updated,
				CurrentRevision: current,
				UpdateRevision:  update,
			},
		}
	}

	cases := []struct {
		name    string
		ss      *appsv1.StatefulSet
		missing bool
		desired int32
		want    string
	}{
		{"missing StatefulSet", &appsv1.StatefulSet{}, true, 3, proxysqlv1alpha1.PhasePending},
		{"zero ready replicas", created(0, 0, "rev-1", "rev-1"), false, 3, proxysqlv1alpha1.PhaseCreating},
		{"all ready, same revision", created(3, 3, "rev-1", "rev-1"), false, 3, proxysqlv1alpha1.PhaseRunning},
		{"rolling update in progress", created(2, 1, "rev-1", "rev-2"), false, 3, proxysqlv1alpha1.PhaseUpdating},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := derivePhase(tc.ss, tc.missing, tc.desired); got != tc.want {
				t.Errorf("derivePhase() = %q, want %q", got, tc.want)
			}
		})
	}
}

// isOwnedBy returns true when obj has owner as a controller ownerReference.
func isOwnedBy(obj metav1.Object, owner *proxysqlv1alpha1.ProxySQLCluster) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.UID && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}
