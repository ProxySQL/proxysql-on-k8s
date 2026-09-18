package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

var _ = Describe("ProxySQLCluster topology validation", func() {
	const ns = "default"

	// mk returns an unpersisted cluster carrying the given topology.
	mk := func(name string, topo *proxysqlv1alpha1.TopologySpec) *proxysqlv1alpha1.ProxySQLCluster {
		return &proxysqlv1alpha1.ProxySQLCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       proxysqlv1alpha1.ProxySQLClusterSpec{Topology: topo},
		}
	}
	coreSatellite := func(mut ...func(*proxysqlv1alpha1.TopologySpec)) *proxysqlv1alpha1.TopologySpec {
		t := &proxysqlv1alpha1.TopologySpec{
			Mode: proxysqlv1alpha1.TopologyModeCoreSatellite,
			Core: proxysqlv1alpha1.CoreSpec{
				Zones: []proxysqlv1alpha1.CoreZone{{Zone: "us-east-1a", Replicas: 2}},
			},
			Satellites: proxysqlv1alpha1.SatellitesSpec{Replicas: 3},
		}
		for _, m := range mut {
			m(t)
		}
		return t
	}

	It("accepts a valid coreSatellite topology", func() {
		ctx := context.Background()
		c := mk("topo-ok", coreSatellite())
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
	})

	It("defaults mode to direct and serveTraffic to true", func() {
		ctx := context.Background()
		c := mk("topo-defaults", &proxysqlv1alpha1.TopologySpec{
			Core: proxysqlv1alpha1.CoreSpec{
				Zones: []proxysqlv1alpha1.CoreZone{{Zone: "us-east-1a", Replicas: 1}},
			},
		})
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
		Expect(c.Spec.Topology.Mode).To(Equal(proxysqlv1alpha1.TopologyModeDirect))
		Expect(c.Spec.Topology.Core.ServeTraffic).NotTo(BeNil())
		Expect(*c.Spec.Topology.Core.ServeTraffic).To(BeTrue())
	})

	It("accepts spec.replicas alongside coreSatellite and ignores it", func() {
		// Replicas carries +kubebuilder:default=3, so the API server
		// materializes it on every create before validation — a rule
		// refusing has(self.replicas) in coreSatellite mode would reject
		// every coreSatellite cluster, including this valid one. The field
		// is simply ignored in this mode; pod counts come from
		// topology.core.zones and topology.satellites.replicas instead.
		ctx := context.Background()
		c := mk("topo-replicas", coreSatellite())
		three := int32(3)
		c.Spec.Replicas = &three
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
		Expect(c.Spec.IsCoreSatellite()).To(BeTrue())
		Expect(*c.Spec.Replicas).To(Equal(three))
		Expect(c.Spec.CoreTotal()).To(Equal(int32(2)))
	})

	It("refuses coreSatellite with no core zones", func() {
		ctx := context.Background()
		c := mk("topo-nozones", coreSatellite(func(t *proxysqlv1alpha1.TopologySpec) {
			t.Core.Zones = nil
		}))
		Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
	})

	It("refuses duplicate core zones", func() {
		ctx := context.Background()
		c := mk("topo-dupzone", coreSatellite(func(t *proxysqlv1alpha1.TopologySpec) {
			t.Core.Zones = []proxysqlv1alpha1.CoreZone{
				{Zone: "us-east-1a", Replicas: 1},
				{Zone: "us-east-1a", Replicas: 1},
			}
		}))
		err := k8sClient.Create(ctx, c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("zone"))
	})

	It("accepts duplicate core zones in direct mode (topology.core is inert there)", func() {
		ctx := context.Background()
		c := mk("topo-dupzone-direct", &proxysqlv1alpha1.TopologySpec{
			Core: proxysqlv1alpha1.CoreSpec{
				Zones: []proxysqlv1alpha1.CoreZone{
					{Zone: "us-east-1a", Replicas: 1},
					{Zone: "us-east-1a", Replicas: 1},
				},
			},
		})
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
		Expect(c.Spec.Topology.Mode).To(Equal(proxysqlv1alpha1.TopologyModeDirect))
	})

	It("refuses serveTraffic=false with zero satellites", func() {
		ctx := context.Background()
		c := mk("topo-noendpoints", coreSatellite(func(t *proxysqlv1alpha1.TopologySpec) {
			f := false
			t.Core.ServeTraffic = &f
			t.Satellites.Replicas = 0
		}))
		err := k8sClient.Create(ctx, c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no client endpoints"))
	})

	It("refuses a core zone with replicas below 1", func() {
		ctx := context.Background()
		c := mk("topo-zerocore", coreSatellite(func(t *proxysqlv1alpha1.TopologySpec) {
			t.Core.Zones = []proxysqlv1alpha1.CoreZone{{Zone: "us-east-1a", Replicas: 0}}
		}))
		Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
	})
})

// ownedStatefulSet is a minimal StatefulSet controlled by the cluster and
// labelled as one of its objects — the shape of a set the topology has
// stopped calling for. It carries a "data" claim template so the prune's PVC
// reclaim has something to match.
func ownedStatefulSet(owner *proxysqlv1alpha1.ProxySQLCluster, n string) *appsv1.StatefulSet {
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: n, Namespace: owner.Namespace,
			Labels: map[string]string{clusterLabel: owner.Name},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(int32(1)),
			ServiceName: owner.Name + "-headless",
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"sts": n}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"sts": n}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "proxysql", Image: "proxysql/proxysql:3.0.11"}}},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
					},
				},
			}},
		},
	}
	ExpectWithOffset(1, controllerutil.SetControllerReference(owner, ss, k8sClient.Scheme())).To(Succeed())
	return ss
}

var _ = Describe("ProxySQLCluster coreSatellite reconcile", func() {
	const ns = "default"
	const name = "pxc-cs"

	var reconciler *ProxySQLClusterReconciler

	// reconcileOnce drives one reconcile of the named cluster.
	reconcileOnce := func(n string) error {
		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: n, Namespace: ns}})
		return err
	}

	get := func(n string) *appsv1.StatefulSet {
		ss := &appsv1.StatefulSet{}
		ExpectWithOffset(1, k8sClient.Get(context.Background(),
			types.NamespacedName{Name: n, Namespace: ns}, ss)).To(Succeed())
		return ss
	}

	// markReady stamps a StatefulSet's status as fully ready. envtest runs no
	// kubelet and no StatefulSet controller, so readiness is only ever what a
	// test writes to the status subresource.
	markReady := func(n string) {
		ss := get(n)
		ss.Status.Replicas = *ss.Spec.Replicas
		ss.Status.ReadyReplicas = *ss.Spec.Replicas
		ss.Status.UpdatedReplicas = *ss.Spec.Replicas
		ExpectWithOffset(1, k8sClient.Status().Update(context.Background(), ss)).To(Succeed())
	}

	roleSets := []string{name + "-core-us-east-1a", name + "-core-us-east-1b", name + "-satellite"}

	BeforeEach(func() {
		ctx := context.Background()
		reconciler = &ProxySQLClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		c := &proxysqlv1alpha1.ProxySQLCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
				Image: proxysqlv1alpha1.ImageSpec{Tag: "3.0.11"},
				Topology: &proxysqlv1alpha1.TopologySpec{
					Mode: proxysqlv1alpha1.TopologyModeCoreSatellite,
					Core: proxysqlv1alpha1.CoreSpec{Zones: []proxysqlv1alpha1.CoreZone{
						{Zone: "us-east-1a", Replicas: 2},
						{Zone: "us-east-1b", Replicas: 1},
					}},
					Satellites: proxysqlv1alpha1.SatellitesSpec{Replicas: 4},
				},
			},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, c)
			for _, n := range append([]string{name, name + "-core-us-east-1c", name + "-foreign"}, roleSets...) {
				_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
			for _, n := range []string{name, name + "-core", name + "-satellite"} {
				_ = k8sClient.Delete(ctx, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
			for _, n := range []string{name, name + "-cnf"} {
				_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
			for _, n := range []string{name, name + "-headless"} {
				_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
		})
		Expect(reconcileOnce(name)).To(Succeed())
	})

	It("creates one StatefulSet per core zone plus the satellite set, and no bare one", func() {
		ctx := context.Background()
		Expect(*get(name + "-core-us-east-1a").Spec.Replicas).To(Equal(int32(2)))
		Expect(*get(name + "-core-us-east-1b").Spec.Replicas).To(Equal(int32(1)))
		Expect(*get(name + "-satellite").Spec.Replicas).To(Equal(int32(4)))

		bare := &appsv1.StatefulSet{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, bare)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "coreSatellite mode must not create the single StatefulSet")
	})

	It("creates both role PDBs and no single-set PDB", func() {
		ctx := context.Background()
		pdb := &policyv1.PodDisruptionBudget{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-core", Namespace: ns}, pdb)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-satellite", Namespace: ns}, pdb)).To(Succeed())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, pdb)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("reports per-zone core status and the satellite count", func() {
		ctx := context.Background()
		c := &proxysqlv1alpha1.ProxySQLCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, c)).To(Succeed())
		Expect(c.Status.Topology).NotTo(BeNil())
		Expect(c.Status.Topology.Mode).To(Equal(proxysqlv1alpha1.TopologyModeCoreSatellite))
		Expect(c.Status.Topology.CoreZones).To(HaveLen(2))
		Expect(c.Status.Topology.CoreZones[0].Zone).To(Equal("us-east-1a"))
		Expect(c.Status.Topology.CoreZones[0].DesiredReplicas).To(Equal(int32(2)))
		Expect(c.Status.Topology.SatelliteReplicas).To(Equal(int32(4)))
		// The aggregate counts every tier: 2 + 1 core + 4 satellites.
		Expect(c.Status.Replicas).To(Equal(int32(7)))

		for _, n := range roleSets {
			markReady(n)
		}
		Expect(reconcileOnce(name)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, c)).To(Succeed())
		Expect(c.Status.Topology.CoreZones[0].ReadyReplicas).To(Equal(int32(2)))
		Expect(c.Status.Topology.CoreZones[1].ReadyReplicas).To(Equal(int32(1)))
		Expect(c.Status.Topology.SatelliteReadyReplicas).To(Equal(int32(4)))
		Expect(c.Status.ReadyReplicas).To(Equal(int32(7)))
		Expect(c.Status.Phase).To(Equal(proxysqlv1alpha1.PhaseRunning))
	})

	It("keeps a dropped zone's StatefulSet until the survivors are Ready, then prunes it with its PVCs", func() {
		ctx := context.Background()
		stale := name + "-core-us-east-1c"

		// A set this cluster owns that the topology no longer calls for...
		owner := &proxysqlv1alpha1.ProxySQLCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, owner)).To(Succeed())
		Expect(k8sClient.Create(ctx, ownedStatefulSet(owner, stale))).To(Succeed())

		// ...and a lookalike carrying the cluster label that this cluster
		// does NOT control (adopted by hand, or another operator's): never
		// ours to delete.
		foreign := ownedStatefulSet(owner, name+"-foreign")
		foreign.OwnerReferences = nil
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		// The pruned set's own PVC, plus a live set's PVC that must survive
		// it (their labels overlap; only the name distinguishes them).
		mkPVC := func(n string) {
			Expect(k8sClient.Create(ctx, &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns, Labels: map[string]string{clusterLabel: name}},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
					},
				},
			})).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			})
		}
		mkPVC("data-" + stale + "-0")
		mkPVC("data-" + name + "-satellite-0")

		// Nothing is Ready yet: pruning now would take the old shape down
		// before the new one serves.
		Expect(reconcileOnce(name)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stale, Namespace: ns}, &appsv1.StatefulSet{})).To(Succeed(),
			"a stale set must survive while the replacements are not Ready")

		for _, n := range roleSets {
			markReady(n)
		}
		Expect(reconcileOnce(name)).To(Succeed())

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: stale, Namespace: ns}, &appsv1.StatefulSet{}))).To(BeTrue(),
			"the stale set must be pruned once every replacement is Ready")
		// The apiserver's StorageObjectInUseProtection admission plugin puts a
		// pvc-protection finalizer on every PVC, and the controller that
		// clears it does not run in envtest — so a deleted PVC lingers with a
		// deletionTimestamp instead of disappearing. Either shape is "gone".
		stalePVC := &corev1.PersistentVolumeClaim{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "data-" + stale + "-0", Namespace: ns}, stalePVC); err == nil {
			Expect(stalePVC.DeletionTimestamp).NotTo(BeNil(), "the pruned set's PVC must go with it")
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		livePVC := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "data-" + name + "-satellite-0", Namespace: ns},
			livePVC)).To(Succeed(), "a live set's PVC must never be swept up by a prune")
		Expect(livePVC.DeletionTimestamp).To(BeNil(), "a live set's PVC must never be swept up by a prune")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-foreign", Namespace: ns},
			&appsv1.StatefulSet{})).To(Succeed(), "a set this cluster does not control must never be pruned")
		for _, n := range roleSets {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: ns}, &appsv1.StatefulSet{})).To(Succeed())
		}
	})

	It("does not prune a dropped zone while the cluster is paused", func() {
		ctx := context.Background()
		stale := name + "-core-us-east-1c"
		owner := &proxysqlv1alpha1.ProxySQLCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, owner)).To(Succeed())
		Expect(k8sClient.Create(ctx, ownedStatefulSet(owner, stale))).To(Succeed())

		// Pause scales every role set to 0, which satisfies readiness
		// vacuously — but pause promises Services, Secrets and PVCs are
		// retained, so a stopped cluster must destroy nothing.
		owner.Spec.Pause = true
		Expect(k8sClient.Update(ctx, owner)).To(Succeed())
		Expect(reconcileOnce(name)).To(Succeed())
		Expect(*get(name + "-satellite").Spec.Replicas).To(Equal(int32(0)))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stale, Namespace: ns}, &appsv1.StatefulSet{})).To(Succeed(),
			"a paused cluster must not prune")

		// Resuming puts the replicas back and the prune resumes with them.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, owner)).To(Succeed())
		owner.Spec.Pause = false
		Expect(k8sClient.Update(ctx, owner)).To(Succeed())
		Expect(reconcileOnce(name)).To(Succeed())
		for _, n := range roleSets {
			markReady(n)
		}
		Expect(reconcileOnce(name)).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: stale, Namespace: ns}, &appsv1.StatefulSet{}))).To(BeTrue())
	})

	It("degrades instead of creating pods when the image predates x.y.8", func() {
		ctx := context.Background()
		old := &proxysqlv1alpha1.ProxySQLCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "pxc-oldimg", Namespace: ns},
			Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
				Image: proxysqlv1alpha1.ImageSpec{Tag: "3.0.7"},
				Topology: &proxysqlv1alpha1.TopologySpec{
					Mode:       proxysqlv1alpha1.TopologyModeCoreSatellite,
					Core:       proxysqlv1alpha1.CoreSpec{Zones: []proxysqlv1alpha1.CoreZone{{Zone: "us-east-1a", Replicas: 1}}},
					Satellites: proxysqlv1alpha1.SatellitesSpec{Replicas: 1},
				},
			},
		}
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, old) })

		Expect(reconcileOnce("pxc-oldimg")).To(HaveOccurred())

		got := &proxysqlv1alpha1.ProxySQLCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "pxc-oldimg", Namespace: ns}, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, condTypeDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("TopologyUnsupportedVersion"))

		ss := &appsv1.StatefulSet{}
		nf := k8sClient.Get(ctx, types.NamespacedName{Name: "pxc-oldimg-core-us-east-1a", Namespace: ns}, ss)
		Expect(apierrors.IsNotFound(nf)).To(BeTrue())
		// Nothing at all was provisioned: not even the auth Secret.
		sec := &corev1.Secret{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: "pxc-oldimg", Namespace: ns}, sec))).To(BeTrue())
	})
})

var _ = Describe("ProxySQLCluster topology conversion markers", func() {
	const ns = "default"
	const name = "pxc-convert"

	// The marker annotations (cnf checksum, vars/structural applied hashes,
	// TLS rotation state) live on the StatefulSets. Mid-conversion the sets
	// the NEW shape names do not exist yet, and reading a missing one hands
	// the engines an empty marker set: the cnf checksum would reset to
	// bootHash, and an empty tls-applied marker makes classifyTLSRotation
	// ADOPT — marking an in-flight rotation applied though no pod ever
	// reloaded the certificate. The read must therefore fall back to
	// whichever set actually exists.
	It("reads the markers off the pre-conversion StatefulSet before any role set exists", func() {
		ctx := context.Background()
		reconciler := &ProxySQLClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		c := &proxysqlv1alpha1.ProxySQLCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       proxysqlv1alpha1.ProxySQLClusterSpec{Image: proxysqlv1alpha1.ImageSpec{Tag: "3.0.11"}},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, c)
			for _, n := range []string{name, name + "-core-us-east-1a", name + "-satellite"} {
				_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
			_ = k8sClient.Delete(ctx, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			for _, n := range []string{name, name + "-cnf"} {
				_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
			for _, n := range []string{name, name + "-headless"} {
				_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
		})
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		// The direct-mode set is the only one that exists. Stamp a sentinel
		// TLS marker on it: an in-flight rotation the conversion must not
		// lose sight of.
		direct := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, direct)).To(Succeed())
		direct.Annotations[annotationTLSAppliedHash] = "sentinel-rotation-hash"
		Expect(k8sClient.Update(ctx, direct)).To(Succeed())
		checksum := direct.Spec.Template.Annotations[annotationCnfChecksum]
		Expect(checksum).NotTo(BeEmpty())

		// Convert to coreSatellite. pxc-convert-core-us-east-1a does not
		// exist yet — this reconcile is the one that creates it.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, c)).To(Succeed())
		c.Spec.Topology = &proxysqlv1alpha1.TopologySpec{
			Mode:       proxysqlv1alpha1.TopologyModeCoreSatellite,
			Core:       proxysqlv1alpha1.CoreSpec{Zones: []proxysqlv1alpha1.CoreZone{{Zone: "us-east-1a", Replicas: 1}}},
			Satellites: proxysqlv1alpha1.SatellitesSpec{Replicas: 1},
		}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())

		b := builders.New(c, k8sClient.Scheme(), builders.Passwords{})
		cur, err := reconciler.currentStatefulSetAnnotations(ctx, b)
		Expect(err).NotTo(HaveOccurred())
		Expect(cur.tlsApplied).To(Equal("sentinel-rotation-hash"),
			"an in-flight rotation must not be lost because the new shape's sets do not exist yet")
		Expect(cur.cnfChecksum).To(Equal(checksum),
			"the cnf checksum must not reset to bootHash mid-conversion")
	})
})

var _ = Describe("ProxySQLCluster direct-mode PDB regression", func() {
	const ns = "default"
	const name = "pxc-direct-pdb"

	// The role-PDB fan-out reconciles three PDBs per pass, two of which are
	// nil in direct mode. A nil that deleted "the PDB named after the
	// cluster" instead of its own name would delete the direct-mode PDB in
	// the same reconcile that created it.
	It("keeps the <cluster> PDB after a reconcile that also evaluates both role PDBs", func() {
		ctx := context.Background()
		reconciler := &ProxySQLClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		c := &proxysqlv1alpha1.ProxySQLCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       proxysqlv1alpha1.ProxySQLClusterSpec{Replicas: ptr.To(int32(3))},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, c)
			_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			_ = k8sClient.Delete(ctx, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			for _, n := range []string{name, name + "-cnf"} {
				_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
			for _, n := range []string{name, name + "-headless"} {
				_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
			}
		})

		for i := range 2 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns},
				&policyv1.PodDisruptionBudget{})).To(Succeed(), "reconcile %d must leave the direct-mode PDB in place", i+1)
		}

		// And the direct-mode cluster's status stays free of a topology block.
		got := &proxysqlv1alpha1.ProxySQLCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.Topology).To(BeNil())
		Expect(got.Status.Replicas).To(Equal(int32(3)))
	})
})
