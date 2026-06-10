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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
)

var _ = Describe("ProxySQLConfig Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		proxysqlconfig := &proxysqlv1alpha1.ProxySQLConfig{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ProxySQLConfig")
			err := k8sClient.Get(ctx, typeNamespacedName, proxysqlconfig)
			if err != nil && errors.IsNotFound(err) {
				resource := &proxysqlv1alpha1.ProxySQLConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
						// Cluster intentionally doesn't exist — the reconciler
						// should surface ClusterFound=False and requeue
						// without erroring out.
						ClusterRef: corev1.LocalObjectReference{Name: "nonexistent"},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &proxysqlv1alpha1.ProxySQLConfig{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ProxySQLConfig")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// The reconcile in the spec body may have added the cleanup
			// finalizer; one more reconcile finalizes (the referenced cluster
			// doesn't exist, so the finalizer is released without cleanup).
			controllerReconciler := &ProxySQLConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			err = k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "config should be gone after finalize")
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ProxySQLConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})

	Context("deletion finalizer", func() {
		const ns = "default"

		reconcileOnce := func(name string) (reconcile.Result, error) {
			r := &ProxySQLConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			return r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
			})
		}

		// makeConfig creates a ProxySQLConfig pointing at clusterName and
		// registers cleanup that strips any finalizer so the suite never
		// accumulates terminating objects.
		makeConfig := func(name, clusterName string, mut ...func(*proxysqlv1alpha1.ProxySQLConfig)) {
			ctx := context.Background()
			c := &proxysqlv1alpha1.ProxySQLConfig{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
					ClusterRef: corev1.LocalObjectReference{Name: clusterName},
				},
			}
			for _, m := range mut {
				m(c)
			}
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			DeferCleanup(func() {
				var cur proxysqlv1alpha1.ProxySQLConfig
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err != nil {
					return
				}
				cur.Finalizers = nil
				_ = k8sClient.Update(ctx, &cur)
				_ = k8sClient.Delete(ctx, &cur)
			})
		}

		getConfig := func(name string) (*proxysqlv1alpha1.ProxySQLConfig, error) {
			var cur proxysqlv1alpha1.ProxySQLConfig
			err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &cur)
			return &cur, err
		}

		It("adds the cleanup finalizer on reconcile", func() {
			const name = "fin-add"
			makeConfig(name, "nonexistent")

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			cur, err := getConfig(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(cur.Finalizers).To(ContainElement("proxysql.com/config-cleanup"))
		})

		It("removes the finalizer on delete when the cluster is absent", func() {
			ctx := context.Background()
			const name = "fin-absent"
			makeConfig(name, "nonexistent")

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			// Re-fetch: the reconcile above added the finalizer, so the
			// original pointer is stale.
			cfg, err := getConfig(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())

			_, err = reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			_, err = getConfig(name)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "config should be gone once the finalizer is released")
		})

		It("honors the skip-cleanup annotation", func() {
			ctx := context.Background()
			const name = "fin-skip"
			makeConfig(name, "nonexistent", func(c *proxysqlv1alpha1.ProxySQLConfig) {
				c.Annotations = map[string]string{"proxysql.com/skip-cleanup": "true"}
			})

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			// Re-fetch: the reconcile above added the finalizer, so the
			// original pointer is stale.
			cfg, err := getConfig(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())

			_, err = reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			_, err = getConfig(name)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "skip-cleanup should release the finalizer without cleanup")
		})

		It("does not remove the finalizer while the cluster exists with no ready pods", func() {
			ctx := context.Background()
			const name = "fin-pending"
			const clusterName = "fin-cluster"

			cluster := &proxysqlv1alpha1.ProxySQLCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

			// The finalize path needs the admin Secret to reach pod discovery.
			b := builders.New(cluster, k8sClient.Scheme(), builders.Passwords{})
			adminSec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: b.SecretName(), Namespace: ns},
				Data: map[string][]byte{
					b.SecretKeys().RadminPassword: []byte("radmin-pass"),
				},
			}
			Expect(k8sClient.Create(ctx, adminSec)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, adminSec) })

			makeConfig(name, clusterName)

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			// Re-fetch: the reconcile above added the finalizer, so the
			// original pointer is stale.
			cfg, err := getConfig(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())

			res, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(5*time.Second),
				"cleanup must be retried while the cluster exists with no ready pods")

			cur, err := getConfig(name)
			Expect(err).NotTo(HaveOccurred(), "config must still exist")
			Expect(cur.Finalizers).To(ContainElement("proxysql.com/config-cleanup"),
				"finalizer must be held until cleanup succeeds or skip-cleanup is set")
		})

		It("retries when cleanup fails on a ready pod", func() {
			ctx := context.Background()
			const name = "fin-retry"
			const clusterName = "fin-cluster-retry"

			cluster := &proxysqlv1alpha1.ProxySQLCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

			b := builders.New(cluster, k8sClient.Scheme(), builders.Passwords{})
			adminSec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: b.SecretName(), Namespace: ns},
				Data: map[string][]byte{
					b.SecretKeys().RadminPassword: []byte("radmin-pass"),
				},
			}
			Expect(k8sClient.Create(ctx, adminSec)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, adminSec) })

			// A Ready pod whose IP is loopback: discoverPodAddresses returns
			// 127.0.0.1:<adminPort>, where nothing listens, so the cleanup
			// SQL connection is refused and applyToReplicas reports 0 cleaned.
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fin-retry-pod",
					Namespace: ns,
					Labels:    map[string]string{"proxysql.com/cluster": clusterName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "proxysql",
						Image: "proxysql/proxysql",
					}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
			})
			pod.Status.PodIP = "127.0.0.1"
			pod.Status.Conditions = []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			makeConfig(name, clusterName)

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			// Re-fetch: the reconcile above added the finalizer, so the
			// original pointer is stale.
			cfg, err := getConfig(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())

			res, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(5*time.Second),
				"partial cleanup must be retried, not released")

			cur, err := getConfig(name)
			Expect(err).NotTo(HaveOccurred(), "config must still exist")
			Expect(cur.Finalizers).To(ContainElement("proxysql.com/config-cleanup"),
				"finalizer must be held while cleanup is failing")
		})
	})

	Context("secret watch mapping", func() {
		const ns = "default"

		// makeConfig creates a ProxySQLConfig pointing at clusterName and
		// registers cleanup that strips any finalizer so the suite never
		// accumulates terminating objects.
		makeConfig := func(name, clusterName string, mut ...func(*proxysqlv1alpha1.ProxySQLConfig)) {
			ctx := context.Background()
			c := &proxysqlv1alpha1.ProxySQLConfig{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
					ClusterRef: corev1.LocalObjectReference{Name: clusterName},
				},
			}
			for _, m := range mut {
				m(c)
			}
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			DeferCleanup(func() {
				var cur proxysqlv1alpha1.ProxySQLConfig
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err != nil {
					return
				}
				cur.Finalizers = nil
				_ = k8sClient.Update(ctx, &cur)
				_ = k8sClient.Delete(ctx, &cur)
			})
		}

		It("maps a referenced password Secret to its ProxySQLConfigs", func() {
			ctx := context.Background()
			makeConfig("secmap-cfg", "secmap-cluster", func(c *proxysqlv1alpha1.ProxySQLConfig) {
				c.Spec.MySQLUsers = []proxysqlv1alpha1.MySQLUser{{
					Username: "app",
					PasswordSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "app-user-pw"},
						Key:                  "password",
					},
				}}
			})

			r := &ProxySQLConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			reqs := r.configsForSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user-pw", Namespace: ns},
			})
			Expect(reqs).To(ContainElement(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "secmap-cfg", Namespace: ns},
			}), "a Secret referenced by passwordSecretRef must map to the config")

			reqs = r.configsForSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "unrelated-pw", Namespace: ns},
			})
			Expect(reqs).NotTo(ContainElement(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "secmap-cfg", Namespace: ns},
			}), "an unrelated Secret must not map to the config")
		})

		It("maps a cluster admin Secret to configs targeting that cluster", func() {
			ctx := context.Background()
			cluster := &proxysqlv1alpha1.ProxySQLCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "secmap-cluster", Namespace: ns},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

			makeConfig("secmap-cfg", "secmap-cluster")

			r := &ProxySQLConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			b := builders.New(cluster, r.Scheme, builders.Passwords{})
			reqs := r.configsForSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: b.SecretName(), Namespace: ns},
			})
			Expect(reqs).To(ContainElement(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "secmap-cfg", Namespace: ns},
			}), "the target cluster's admin Secret must map to the config")
		})
	})
})
