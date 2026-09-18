package builders

import (
	"reflect"
	"strings"
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

func TestCoreStatefulSets_PinnedPerZone(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)
	sets := b.CoreStatefulSets("chk123")
	if len(sets) != 2 {
		t.Fatalf("got %d core StatefulSets, want 2", len(sets))
	}

	first := sets[0]
	if first.Name != "pxc-core-us-east-1a" {
		t.Errorf("name = %q, want pxc-core-us-east-1a", first.Name)
	}
	if *first.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *first.Spec.Replicas)
	}
	if first.Spec.ServiceName != "pxc-headless" {
		t.Errorf("serviceName = %q, want pxc-headless", first.Spec.ServiceName)
	}
	if got := first.Spec.Selector.MatchLabels["proxysql.com/core-zone"]; got != "us-east-1a" {
		t.Errorf("selector core-zone = %q, want us-east-1a", got)
	}
	if got := first.Spec.Template.Labels["proxysql.com/role"]; got != "core" {
		t.Errorf("pod role label = %q, want core", got)
	}
	if got := first.Spec.Template.Annotations["proxysql.com/cnf-checksum"]; got != "chk123" {
		t.Errorf("cnf-checksum = %q, want chk123", got)
	}

	// Hard zone pin, not a spread preference.
	terms := first.Spec.Template.Spec.Affinity.NodeAffinity.
		RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
		t.Fatalf("expected one required nodeAffinity term, got %+v", terms)
	}
	expr := terms[0].MatchExpressions[0]
	if expr.Key != "topology.kubernetes.io/zone" || expr.Values[0] != "us-east-1a" {
		t.Errorf("nodeAffinity = %s in %v, want zone us-east-1a", expr.Key, expr.Values)
	}
	if sets[1].Name != "pxc-core-us-east-1b" || *sets[1].Spec.Replicas != 1 {
		t.Errorf("second core set = %s/%d, want pxc-core-us-east-1b/1", sets[1].Name, *sets[1].Spec.Replicas)
	}
}

func TestSatelliteStatefulSet(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)
	ss := b.SatelliteStatefulSet("chk123")
	if ss.Name != "pxc-satellite" {
		t.Errorf("name = %q, want pxc-satellite", ss.Name)
	}
	if *ss.Spec.Replicas != 4 {
		t.Errorf("replicas = %d, want 4", *ss.Spec.Replicas)
	}
	if got := ss.Spec.Selector.MatchLabels["proxysql.com/role"]; got != "satellite" {
		t.Errorf("selector role = %q, want satellite", got)
	}
	if ss.Spec.Template.Spec.Affinity != nil &&
		ss.Spec.Template.Spec.Affinity.NodeAffinity != nil {
		t.Error("satellites must not be zone-pinned; placement comes from spec.affinity")
	}
}

func TestCoreStatefulSets_PausedScalesToZero(t *testing.T) {
	c := coreSatelliteCluster()
	c.Spec.Pause = true
	b := New(c, newScheme(t), goldenPasswords)
	for _, ss := range b.CoreStatefulSets("chk") {
		if *ss.Spec.Replicas != 0 {
			t.Errorf("%s replicas = %d while paused, want 0", ss.Name, *ss.Spec.Replicas)
		}
	}
	if *b.SatelliteStatefulSet("chk").Spec.Replicas != 0 {
		t.Error("satellite replicas != 0 while paused")
	}
}

func TestDirectModeStatefulSetUnchanged(t *testing.T) {
	c := coreSatelliteCluster()
	c.Spec.Topology = nil
	three := int32(3)
	c.Spec.Replicas = &three
	b := New(c, newScheme(t), goldenPasswords)

	ss := b.StatefulSet("chk")
	if ss.Name != "pxc" {
		t.Errorf("direct StatefulSet name = %q, want pxc", ss.Name)
	}
	for _, k := range []string{"proxysql.com/role", "proxysql.com/core-zone"} {
		if _, ok := ss.Spec.Selector.MatchLabels[k]; ok {
			t.Errorf("direct-mode selector gained %q", k)
		}
		if _, ok := ss.Spec.Template.Labels[k]; ok {
			t.Errorf("direct-mode pod template gained %q", k)
		}
	}
	if len(b.CoreStatefulSets("chk")) != 0 {
		t.Error("CoreStatefulSets must be empty in direct mode")
	}
	if b.SatelliteStatefulSet("chk") != nil {
		t.Error("SatelliteStatefulSet must be nil in direct mode")
	}
}

func TestBootstrapCnf_CoreSatelliteSyncKeys(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)
	cnf, err := b.BootstrapCnf(b.CorePodDNS())
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	// Every pod — core and satellite — gets the SAME peer list: the cores.
	for _, want := range []string{
		`hostname="pxc-core-us-east-1a-0.pxc-headless.ns1.svc"`,
		`hostname="pxc-core-us-east-1b-0.pxc-headless.ns1.svc"`,
	} {
		if !strings.Contains(cnf, want) {
			t.Errorf("cnf missing peer %s", want)
		}
	}
	if strings.Contains(cnf, "pxc-satellite") {
		t.Error("satellites must never appear in proxysql_servers")
	}

	// Native sync must cover the modules the operator stops writing in
	// #2319: variables and the pgsql tables.
	for _, want := range []string{
		"cluster_mysql_variables_diffs_before_sync",
		"cluster_admin_variables_diffs_before_sync",
		"cluster_pgsql_servers_diffs_before_sync",
		"cluster_pgsql_users_diffs_before_sync",
		"cluster_pgsql_query_rules_diffs_before_sync",
		"cluster_pgsql_variables_diffs_before_sync",
		"cluster_pgsql_servers_save_to_disk",
	} {
		if !strings.Contains(cnf, want) {
			t.Errorf("cnf missing %s", want)
		}
	}
}

func TestBootstrapCnf_DirectModeHasNoExtraSyncKeys(t *testing.T) {
	c := coreSatelliteCluster()
	c.Spec.Topology = nil
	three := int32(3)
	c.Spec.Replicas = &three
	b := New(c, newScheme(t), goldenPasswords)

	cnf, err := b.BootstrapCnf(b.ProxySQLServerDNS())
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	// Adding these in direct mode would change the cnf of every existing
	// multi-replica cluster and roll it once on upgrade.
	for _, absent := range []string{
		"cluster_mysql_variables_diffs_before_sync",
		"cluster_pgsql_servers_diffs_before_sync",
	} {
		if strings.Contains(cnf, absent) {
			t.Errorf("direct-mode cnf gained %s — existing clusters would roll on upgrade", absent)
		}
	}
}

func TestCnfSecret_CoreSatelliteSeedsCorePeers(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)
	sec, err := b.CnfSecret()
	if err != nil {
		t.Fatalf("CnfSecret: %v", err)
	}
	if !strings.Contains(string(sec.Data["proxysql.cnf"]), "pxc-core-us-east-1a-0") {
		t.Error("cnf Secret does not carry the core peer list")
	}
}

func TestClientServiceSelector(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)
	// serveTraffic defaults true: the client Service selects every pod, so
	// it must NOT carry a role key.
	if _, ok := b.Service().Spec.Selector["proxysql.com/role"]; ok {
		t.Error("serveTraffic=true selector must not pin a role")
	}

	c := coreSatelliteCluster()
	f := false
	c.Spec.Topology.Core.ServeTraffic = &f
	b2 := New(c, newScheme(t), goldenPasswords)
	if got := b2.Service().Spec.Selector["proxysql.com/role"]; got != "satellite" {
		t.Errorf("serveTraffic=false selector role = %q, want satellite", got)
	}
	// The headless Service always covers every pod: it is the StatefulSets'
	// serviceName and the DNS cluster sync relies on.
	if _, ok := b2.HeadlessService().Spec.Selector["proxysql.com/role"]; ok {
		t.Error("headless Service must select every pod regardless of role")
	}
}

func TestRolePDBs(t *testing.T) {
	b := New(coreSatelliteCluster(), newScheme(t), goldenPasswords)

	core := b.CorePDB()
	if core == nil {
		t.Fatal("CorePDB = nil, want a PDB for 3 core pods")
	}
	if core.Name != "pxc-core" {
		t.Errorf("core PDB name = %q, want pxc-core", core.Name)
	}
	// Cluster-wide, not per zone: at most one core unavailable anywhere.
	if core.Spec.MaxUnavailable == nil || core.Spec.MaxUnavailable.IntValue() != 1 {
		t.Errorf("core PDB maxUnavailable = %v, want 1", core.Spec.MaxUnavailable)
	}
	if got := core.Spec.Selector.MatchLabels["proxysql.com/role"]; got != "core" {
		t.Errorf("core PDB selects role %q, want core", got)
	}
	if _, ok := core.Spec.Selector.MatchLabels["proxysql.com/core-zone"]; ok {
		t.Error("core PDB must span all zones, not one")
	}

	sat := b.SatellitePDB()
	if sat == nil || sat.Name != "pxc-satellite" {
		t.Fatalf("SatellitePDB = %v, want pxc-satellite", sat)
	}
	if sat.Spec.MinAvailable == nil || sat.Spec.MinAvailable.IntValue() != 3 {
		t.Errorf("satellite PDB minAvailable = %v, want 3 (replicas-1)", sat.Spec.MinAvailable)
	}

	// The single-StatefulSet PDB must not also exist in this mode.
	if b.PodDisruptionBudget() != nil {
		t.Error("PodDisruptionBudget() must be nil in coreSatellite mode")
	}
}

func TestRolePDBs_NilInDirectMode(t *testing.T) {
	c := coreSatelliteCluster()
	c.Spec.Topology = nil
	three := int32(3)
	c.Spec.Replicas = &three
	b := New(c, newScheme(t), goldenPasswords)
	if b.CorePDB() != nil || b.SatellitePDB() != nil {
		t.Error("role PDBs must be nil in direct mode")
	}
	if b.PodDisruptionBudget() == nil {
		t.Error("direct mode still needs its single PDB")
	}
}
