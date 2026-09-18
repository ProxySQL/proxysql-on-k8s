package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
