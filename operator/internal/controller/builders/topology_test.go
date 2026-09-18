package builders

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// coreSatelliteCluster returns a 2+1 core, 4 satellite cluster in ns "ns1".
func coreSatelliteCluster() *proxysqlv1alpha1.ProxySQLCluster {
	return &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pxc", Namespace: "ns1"},
		Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
			Topology: &proxysqlv1alpha1.TopologySpec{
				Mode: proxysqlv1alpha1.TopologyModeCoreSatellite,
				Core: proxysqlv1alpha1.CoreSpec{
					Zones: []proxysqlv1alpha1.CoreZone{
						{Zone: "us-east-1a", Replicas: 2},
						{Zone: "us-east-1b", Replicas: 1},
					},
				},
				Satellites: proxysqlv1alpha1.SatellitesSpec{Replicas: 4},
			},
		},
	}
}

func TestCoreStatefulSetName(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)
	if got, want := b.CoreStatefulSetName("us-east-1a"), "pxc-core-us-east-1a"; got != want {
		t.Errorf("CoreStatefulSetName = %q, want %q", got, want)
	}
	if got, want := b.SatelliteStatefulSetName(), "pxc-satellite"; got != want {
		t.Errorf("SatelliteStatefulSetName = %q, want %q", got, want)
	}
}

func TestRoleSelectorLabels(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)

	core := b.RoleSelectorLabels(RoleCore, "us-east-1a")
	want := map[string]string{
		"app.kubernetes.io/name":     "proxysql",
		"app.kubernetes.io/instance": "pxc",
		"proxysql.com/cluster":       "pxc",
		"proxysql.com/role":          "core",
		"proxysql.com/core-zone":     "us-east-1a",
	}
	if !reflect.DeepEqual(core, want) {
		t.Errorf("core selector = %v, want %v", core, want)
	}

	sat := b.RoleSelectorLabels(RoleSatellite, "")
	if _, ok := sat["proxysql.com/core-zone"]; ok {
		t.Errorf("satellite selector must not carry a core-zone label: %v", sat)
	}
	if sat["proxysql.com/role"] != "satellite" {
		t.Errorf("satellite role label = %q, want satellite", sat["proxysql.com/role"])
	}

	// The shared SelectorLabels() must stay untouched: it is the immutable
	// selector of every pre-topology cluster's StatefulSet.
	shared := b.SelectorLabels()
	if _, ok := shared["proxysql.com/role"]; ok {
		t.Errorf("SelectorLabels() must not gain a role key: %v", shared)
	}
}

func TestCorePodDNS(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)
	got := b.CorePodDNS()
	want := []string{
		"pxc-core-us-east-1a-0.pxc-headless.ns1.svc",
		"pxc-core-us-east-1a-1.pxc-headless.ns1.svc",
		"pxc-core-us-east-1b-0.pxc-headless.ns1.svc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CorePodDNS = %v, want %v", got, want)
	}
}

func TestCorePodDNS_DirectModeIsEmpty(t *testing.T) {
	c := coreSatelliteCluster()
	c.Spec.Topology = nil
	b := New(c, newScheme(t), goldenPasswords)
	if got := b.CorePodDNS(); got != nil {
		t.Errorf("CorePodDNS in direct mode = %v, want nil", got)
	}
}

func TestServeTrafficFromCore_DefaultsTrue(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)
	if !b.ServeTrafficFromCore() {
		t.Error("ServeTrafficFromCore = false, want true when serveTraffic is unset")
	}
	c := coreSatelliteCluster()
	f := false
	c.Spec.Topology.Core.ServeTraffic = &f
	b2 := New(c, newScheme(t), goldenPasswords)
	if b2.ServeTrafficFromCore() {
		t.Error("ServeTrafficFromCore = true, want false when serveTraffic=false")
	}
}
