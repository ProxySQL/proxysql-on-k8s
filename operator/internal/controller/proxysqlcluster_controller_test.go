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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-cnf", Namespace: ns}},
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

		It("creates a Secret containing the bootstrap proxysql.cnf and no ConfigMap", func() {
			ctx := context.Background()
			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &sec)).To(Succeed())
			Expect(sec.Data).To(HaveKey("proxysql.cnf"))
			Expect(string(sec.Data["proxysql.cnf"])).To(ContainSubstring("admin_credentials="))
			Expect(string(sec.Data["proxysql.cnf"])).To(ContainSubstring("proxysql_servers="), "replicas=3 should populate cluster sync")
			Expect(isOwnedBy(&sec, cluster)).To(BeTrue())

			// The cnf carries passwords; it must no longer land in a ConfigMap.
			var cm corev1.ConfigMap
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &cm)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no bootstrap ConfigMap should be created")
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

	When("the user sets networking knobs (#28)", func() {
		const name = "pxc-netknobs"

		It("applies service annotations, session affinity, and keepalive sysctls — and updates annotations on change", func() {
			ctx := context.Background()
			timeout := int32(300)
			kaTime := int32(120)
			cluster = makeCluster(name, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.Service.Annotations = map[string]string{"example.com/lb": "internal"}
				c.Spec.Service.SessionAffinityTimeoutSeconds = &timeout
				c.Spec.Networking.TCPKeepalive.Time = &kaTime
			})
			reconcileAndExpectSuccess(name)

			var svc, headless corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &svc)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-headless", Namespace: ns}, &headless)).To(Succeed())

			Expect(svc.Annotations).To(HaveKeyWithValue("example.com/lb", "internal"))
			Expect(svc.Spec.SessionAffinity).To(Equal(corev1.ServiceAffinityClientIP))
			Expect(svc.Spec.SessionAffinityConfig).NotTo(BeNil())
			Expect(*svc.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds).To(Equal(int32(300)))

			// The headless Service stays untouched by spec.service.
			Expect(headless.Annotations).NotTo(HaveKey("example.com/lb"))
			Expect(headless.Spec.SessionAffinity).NotTo(Equal(corev1.ServiceAffinityClientIP))

			var ss appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &ss)).To(Succeed())
			Expect(ss.Spec.Template.Spec.SecurityContext.Sysctls).To(ConsistOf(
				corev1.Sysctl{Name: "net.ipv4.tcp_keepalive_time", Value: "120"},
			))

			// Change the annotation value: ensureService must propagate it
			// (annotations live in ObjectMeta, not in the copied Spec).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Service.Annotations = map[string]string{"example.com/lb": "internet-facing"}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			reconcileAndExpectSuccess(name)

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &svc)).To(Succeed())
			Expect(svc.Annotations).To(HaveKeyWithValue("example.com/lb", "internet-facing"))

			// Merge semantics contract: a key removed from the spec lingers
			// on the Service (the operator cannot tell a removed spec key
			// from one written by a cloud controller, and wiping foreign
			// annotations would fight LB controllers).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Service.Annotations = nil
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			reconcileAndExpectSuccess(name)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &svc)).To(Succeed())
			Expect(svc.Annotations).To(HaveKey("example.com/lb"),
				"removed spec annotation must linger on the Service (merge semantics)")
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
			var cnfSec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &cnfSec)).To(Succeed())
			Expect(string(cnfSec.Data["proxysql.cnf"])).To(ContainSubstring("admin:admin-from-user"))

			// The operator must NOT have created a Secret named after the cluster.
			var clusterNamedSec corev1.Secret
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &clusterNamedSec)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "operator should not mint a secret when SecretName is set")
		})
	})

	When("the user provides a username/password-shaped auth Secret", func() {
		const name = "pxc-platform-secret"
		const secretName = "platform-admin-creds"

		It("derives admin passwords and adds the extra admin credential to the cnf", func() {
			ctx := context.Background()
			externalSec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
				Data: map[string][]byte{
					"username": []byte("platform"),
					"password": []byte("s3cret"),
				},
			}
			Expect(k8sClient.Create(ctx, externalSec)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, externalSec) })

			cluster = makeCluster(name, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.Auth.SecretName = secretName
			})
			reconcileAndExpectSuccess(name)

			// No Degraded condition: the secret resolved.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			Expect(meta.FindStatusCondition(cluster.Status.Conditions, condTypeDegraded)).To(BeNil())

			// admin/radmin share the platform password, and the platform's own
			// username rides along as a remote-capable admin credential.
			var cnfSec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &cnfSec)).To(Succeed())
			Expect(string(cnfSec.Data["proxysql.cnf"])).To(ContainSubstring(
				`admin_credentials="admin:s3cret;radmin:s3cret;platform:s3cret"`))

			// The operator must NOT have minted its own Secret.
			var clusterNamedSec corev1.Secret
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &clusterNamedSec)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	When("the external auth Secret matches neither schema", func() {
		const name = "pxc-bad-secret"
		const secretName = "bad-admin-creds"

		It("degrades with AuthSecretError naming both accepted schemas", func() {
			ctx := context.Background()
			externalSec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
				Data:       map[string][]byte{"foo": []byte("bar")},
			}
			Expect(k8sClient.Create(ctx, externalSec)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, externalSec) })

			cluster = makeCluster(name, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.Auth.SecretName = secretName
			})
			req = reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).To(HaveOccurred())

			var got proxysqlv1alpha1.ProxySQLCluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(proxysqlv1alpha1.PhaseDegraded))
			degraded := meta.FindStatusCondition(got.Status.Conditions, condTypeDegraded)
			Expect(degraded).NotTo(BeNil())
			Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
			Expect(degraded.Reason).To(Equal("AuthSecretError"))
			Expect(degraded.Message).To(ContainSubstring(builders.SecretKeyAdminPassword))
			Expect(degraded.Message).To(ContainSubstring("username/password"))
		})
	})

	When("a legacy operator-owned bootstrap ConfigMap exists (pre-Secret upgrade)", func() {
		const name = "pxc-cm-migration"

		It("deletes the owned ConfigMap but leaves a foreign one alone", func() {
			ctx := context.Background()
			cluster = makeCluster(name)

			// Simulate a v0.2.x leftover: a ConfigMap named after the cluster,
			// controller-owned by it (as the old ensureConfigMap produced).
			legacy := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Data:       map[string]string{"proxysql.cnf": "# legacy"},
			}
			Expect(controllerutil.SetControllerReference(cluster, legacy, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, legacy)).To(Succeed())

			reconcileAndExpectSuccess(name)

			var cm corev1.ConfigMap
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &cm)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "legacy owned ConfigMap should be garbage-collected by the reconciler")
		})

		It("does not delete a same-named ConfigMap it does not own", func() {
			ctx := context.Background()
			const foreignName = "pxc-cm-foreign"
			foreign := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: foreignName, Namespace: ns},
				Data:       map[string]string{"something": "user-managed"},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

			cluster = makeCluster(foreignName)
			reconcileAndExpectSuccess(foreignName)

			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: foreignName, Namespace: ns}, &cm)).To(Succeed(),
				"a user-managed ConfigMap that merely shares the name must survive")
		})
	})

	When("the cnf content changes", func() {
		const name = "pxc-cnf-rollout"

		It("updates the cnf Secret and rolls the checksum annotation", func() {
			ctx := context.Background()
			cluster = makeCluster(name)
			reconcileAndExpectSuccess(name)

			var before appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &before)).To(Succeed())
			checksumBefore := before.Spec.Template.Annotations["proxysql.com/cnf-checksum"]
			Expect(checksumBefore).NotTo(BeEmpty())

			// Enabling the web UI changes the rendered cnf.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Protocols.Web.Enabled = ptrBool(true)
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			reconcileAndExpectSuccess(name)

			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &sec)).To(Succeed())
			Expect(string(sec.Data["proxysql.cnf"])).To(ContainSubstring(`web_enabled="true"`))

			var after appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &after)).To(Succeed())
			checksumAfter := after.Spec.Template.Annotations["proxysql.com/cnf-checksum"]
			Expect(checksumAfter).NotTo(Equal(checksumBefore), "cnf change must roll the checksum annotation")
			Expect(checksumAfter).To(Equal(builders.CnfChecksum(sec.Data)),
				"annotation must be the deterministic SHA-256 over every cnf Secret key")
		})
	})

	When("a spec.variables change is variables-only (restart-free runtime apply)", func() {
		const name = "pxc-runtime-vars"

		It("updates the cnf Secret but leaves the pod-template checksum annotation unchanged", func() {
			ctx := context.Background()
			cluster = makeCluster(name, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.Variables.MySQL = map[string]string{"mysql-max_connections": "700"}
			})
			reconcileAndExpectSuccess(name)

			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &sec)).To(Succeed())
			Expect(string(sec.Data["proxysql.cnf"])).To(ContainSubstring(`max_connections="700"`))

			var before appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &before)).To(Succeed())
			checksumBefore := before.Spec.Template.Annotations[annotationCnfChecksum]
			Expect(checksumBefore).NotTo(BeEmpty())

			// envtest never brings up a real kubelet, so no pod ever goes
			// Ready — discoverPodAddresses returns no addresses and
			// resolveRestartChecksum takes the "nothing running yet" branch,
			// keeping the previous annotation. Production behaves the same
			// way when no replica is reachable; when replicas ARE reachable
			// it dials radmin and applies the diff via SQL instead (covered
			// by the pure classifyCnfChange unit tests + proxysqlclient's
			// own ApplyVariables/ReadGlobalVariables tests).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Variables.MySQL = map[string]string{"mysql-max_connections": "701"}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			reconcileAndExpectSuccess(name)

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &sec)).To(Succeed())
			Expect(string(sec.Data["proxysql.cnf"])).To(ContainSubstring(`max_connections="701"`),
				"the cnf Secret must still reflect the new value")

			var after appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &after)).To(Succeed())
			Expect(after.Spec.Template.Annotations[annotationCnfChecksum]).To(Equal(checksumBefore),
				"a variables-only cnf change must not roll the pod-template checksum annotation")
		})
	})

	When("a spec change is structural (adds the proxysql_servers block)", func() {
		const name = "pxc-runtime-structural"

		It("rolls the pod-template checksum annotation to the new CnfChecksum", func() {
			ctx := context.Background()
			one := int32(1)
			cluster = makeCluster(name, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.Replicas = &one
			})
			reconcileAndExpectSuccess(name)

			var before appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &before)).To(Succeed())
			checksumBefore := before.Spec.Template.Annotations[annotationCnfChecksum]
			Expect(checksumBefore).NotTo(BeEmpty())

			// Scaling past 1 replica adds the proxysql_servers block to the
			// rendered cnf — structural, not a variable-value change.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			three := int32(3)
			cluster.Spec.Replicas = &three
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			reconcileAndExpectSuccess(name)

			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &sec)).To(Succeed())
			Expect(string(sec.Data["proxysql.cnf"])).To(ContainSubstring("proxysql_servers="))

			var after appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &after)).To(Succeed())
			checksumAfter := after.Spec.Template.Annotations[annotationCnfChecksum]
			Expect(checksumAfter).NotTo(Equal(checksumBefore),
				"a structural cnf change must roll the pod-template checksum annotation")
			Expect(checksumAfter).To(Equal(builders.CnfChecksum(sec.Data)),
				"the rolled annotation must be the new deterministic CnfChecksum")
		})
	})

	When("a fluent-bit-only Secret key changes (logging sink host)", func() {
		const name = "pxc-flb-only-change"

		It("rolls the pod-template checksum annotation even though proxysql.cnf is unchanged", func() {
			ctx := context.Background()
			cluster = makeCluster(name, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.Logging = &proxysqlv1alpha1.LoggingSpec{
					Enabled: true, QueryLog: true, SinkType: "http",
					HTTP: &proxysqlv1alpha1.HTTPSinkSpec{Host: "collector-a.example.com"},
				}
			})
			reconcileAndExpectSuccess(name)

			var secBefore corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &secBefore)).To(Succeed())
			Expect(string(secBefore.Data["fluent-bit.conf"])).To(ContainSubstring("collector-a.example.com"))
			cnfBefore := string(secBefore.Data["proxysql.cnf"])

			var before appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &before)).To(Succeed())
			checksumBefore := before.Spec.Template.Annotations[annotationCnfChecksum]
			Expect(checksumBefore).NotTo(BeEmpty())

			// Changing only the HTTP sink host rewrites fluent-bit.conf but
			// leaves proxysql.cnf byte-identical (the cnf depends on queryLog,
			// not the sink). Regression pin: a diff limited to a
			// non-proxysql.cnf key must still take the restart path, or the
			// sidecar keeps running with stale config forever.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Logging.HTTP.Host = "collector-b.example.com"
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			reconcileAndExpectSuccess(name)

			var secAfter corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &secAfter)).To(Succeed())
			Expect(string(secAfter.Data["fluent-bit.conf"])).To(ContainSubstring("collector-b.example.com"))
			Expect(string(secAfter.Data["proxysql.cnf"])).To(Equal(cnfBefore),
				"test premise: the sink-host change must not touch proxysql.cnf")

			var after appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &after)).To(Succeed())
			checksumAfter := after.Spec.Template.Annotations[annotationCnfChecksum]
			Expect(checksumAfter).NotTo(Equal(checksumBefore),
				"a fluent-bit.conf-only change must roll the checksum annotation (stale sidecar config otherwise)")
			Expect(checksumAfter).To(Equal(builders.CnfChecksum(secAfter.Data)))
		})
	})

	When("a StatefulSet carries a legacy-scheme checksum annotation and the cnf is unchanged", func() {
		const name = "pxc-legacy-annotation"

		It("keeps the legacy annotation verbatim instead of adopting the new-scheme hash", func() {
			ctx := context.Background()
			cluster = makeCluster(name)
			reconcileAndExpectSuccess(name)

			// Simulate an annotation written under a pre-runtime-reconfig
			// scheme (e.g. a bare hash of proxysql.cnf alone, not
			// builders.CnfChecksum's hash over every Secret key).
			var ss appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &ss)).To(Succeed())
			ss.Spec.Template.Annotations[annotationCnfChecksum] = "legacy-scheme-checksum-value"
			Expect(k8sClient.Update(ctx, &ss)).To(Succeed())

			// No spec change at all: the rendered cnf is byte-identical.
			reconcileAndExpectSuccess(name)

			var after appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &after)).To(Succeed())
			Expect(after.Spec.Template.Annotations[annotationCnfChecksum]).To(Equal("legacy-scheme-checksum-value"),
				"an unchanged cnf must never force adoption of the new checksum scheme")
		})
	})

	When("spec.logging is toggled", func() {
		const name = "pxc-logging"

		It("adds and removes the fluent-bit sidecar and rolls the checksum", func() {
			ctx := context.Background()
			cluster = makeCluster(name)
			reconcileAndExpectSuccess(name)

			var before appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &before)).To(Succeed())
			Expect(before.Spec.Template.Spec.Containers).To(HaveLen(1), "logging off: no sidecar")
			checksumBefore := before.Spec.Template.Annotations["proxysql.com/cnf-checksum"]

			// Enable the sidecar (queryLog must come along per the CEL rule).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Logging = &proxysqlv1alpha1.LoggingSpec{Enabled: true, QueryLog: true}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			reconcileAndExpectSuccess(name)

			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &sec)).To(Succeed())
			Expect(sec.Data).To(HaveKey("fluent-bit.conf"))
			Expect(string(sec.Data["proxysql.cnf"])).To(ContainSubstring("eventslog_filename"))

			var after appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &after)).To(Succeed())
			containerNames := make([]string, 0, 2)
			for _, c := range after.Spec.Template.Spec.Containers {
				containerNames = append(containerNames, c.Name)
			}
			Expect(containerNames).To(ConsistOf("proxysql", "fluent-bit"))
			Expect(after.Spec.Template.Annotations["proxysql.com/cnf-checksum"]).NotTo(Equal(checksumBefore),
				"enabling logging must roll the checksum (cnf + fluent-bit.conf changed)")

			// Disable again: sidecar and conf key disappear.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Logging = nil
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			reconcileAndExpectSuccess(name)

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-cnf", Namespace: ns}, &sec)).To(Succeed())
			Expect(sec.Data).NotTo(HaveKey("fluent-bit.conf"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &after)).To(Succeed())
			Expect(after.Spec.Template.Spec.Containers).To(HaveLen(1), "logging off again: sidecar removed")
		})

		It("rejects logging.enabled without queryLog at admission", func() {
			ctx := context.Background()
			bad := &proxysqlv1alpha1.ProxySQLCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "pxc-logging-bad", Namespace: ns},
				Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
					Logging: &proxysqlv1alpha1.LoggingSpec{Enabled: true},
				},
			}
			err := k8sClient.Create(ctx, bad)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("queryLog"),
				"CEL rule must name queryLog as the missing input")
		})

		It("rejects sinkType=s3 and sinkType=http without their sink blocks", func() {
			ctx := context.Background()
			for _, sink := range []string{"s3", "http"} {
				bad := &proxysqlv1alpha1.ProxySQLCluster{
					ObjectMeta: metav1.ObjectMeta{Name: "pxc-logging-" + sink, Namespace: ns},
					Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
						Logging: &proxysqlv1alpha1.LoggingSpec{
							Enabled: true, QueryLog: true, SinkType: sink,
						},
					},
				}
				err := k8sClient.Create(ctx, bad)
				Expect(err).To(HaveOccurred(), "sinkType=%s without %s block must be rejected", sink, sink)
				Expect(err.Error()).To(ContainSubstring(sink))
			}
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
		// All replicas still ready but a new revision just landed: the
		// revision mismatch alone must flip the phase to Updating.
		{"all ready, revision mismatch", created(3, 3, "rev-1", "rev-2"), false, 3, proxysqlv1alpha1.PhaseUpdating},
		// Fresh StatefulSet whose UpdateRevision the controller hasn't
		// populated yet counts as "no update in flight".
		{"all ready, empty UpdateRevision", created(3, 3, "rev-1", ""), false, 3, proxysqlv1alpha1.PhaseRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := derivePhase(tc.ss, tc.missing, tc.desired); got != tc.want {
				t.Errorf("derivePhase() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ptrBool returns a pointer to v.
func ptrBool(v bool) *bool { return &v }

// isOwnedBy returns true when obj has owner as a controller ownerReference.
func isOwnedBy(obj metav1.Object, owner *proxysqlv1alpha1.ProxySQLCluster) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.UID && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}
