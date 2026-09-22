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
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
)

// newCleanupTestBuilder returns a Builder for a cluster named "cleanup-test"
// with the given replica count, mirroring the construction pattern used by
// renderCnf in restart_checksum_test.go.
func newCleanupTestBuilder(replicas *int32) *builders.Builder {
	c := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-test", Namespace: "default"},
		Spec:       proxysqlv1alpha1.ProxySQLClusterSpec{Replicas: replicas},
	}
	return builders.New(c, nil, defaultPw)
}

// cleanupDesired must preserve the operator-populated proxysql_servers peer
// list (#42): deleting a ProxySQLConfig used to push a fully empty Desired,
// which DELETEs proxysql_servers even though the target cluster (and its
// need to peer via ProxySQL Cluster) still exists.
func TestCleanupDesired_PreservesAutoPopulatedPeers_MultiReplica(t *testing.T) {
	b := newCleanupTestBuilder(int32Ptr(3))

	d := cleanupDesired(b, true)

	wantHosts := b.ProxySQLServerDNS()
	if len(wantHosts) != 3 {
		t.Fatalf("test setup: ProxySQLServerDNS() = %v, want 3 entries", wantHosts)
	}
	if len(d.ProxySQLServers) != len(wantHosts) {
		t.Fatalf("ProxySQLServers = %v, want %d entries derived from %v", d.ProxySQLServers, len(wantHosts), wantHosts)
	}
	for i, host := range wantHosts {
		got := d.ProxySQLServers[i]
		if got.Hostname != host {
			t.Errorf("ProxySQLServers[%d].Hostname = %q, want %q", i, got.Hostname, host)
		}
		if got.Port != b.Spec.Protocols.Admin.Port {
			t.Errorf("ProxySQLServers[%d].Port = %d, want %d", i, got.Port, b.Spec.Protocols.Admin.Port)
		}
		if got.Comment != autoPopulatedPeerComment {
			t.Errorf("ProxySQLServers[%d].Comment = %q, want %q", i, got.Comment, autoPopulatedPeerComment)
		}
	}
}

// A single-replica cluster has no peers to preserve: ProxySQLServerDNS
// returns nil, so cleanup legitimately clears proxysql_servers.
func TestCleanupDesired_SingleReplica_EmptyPeerList(t *testing.T) {
	b := newCleanupTestBuilder(int32Ptr(1))

	d := cleanupDesired(b, true)

	if len(d.ProxySQLServers) != 0 {
		t.Errorf("ProxySQLServers = %v, want empty for a single-replica cluster", d.ProxySQLServers)
	}
}

// When the deleted config carried an EXPLICIT spec.proxysqlServers list
// (autoPopulated=false), cleanup must NOT substitute the auto-derived
// in-cluster peer rows: the documented semantics of an explicit list are
// that it fully replaces auto-population — it exists precisely for
// topologies the operator cannot derive (e.g. peers outside this cluster).
// Fabricating per-pod DNS names on deletion would overwrite a custom
// topology with wrong peers; the correct cleanup is the pre-#42 behavior
// of clearing the table.
func TestCleanupDesired_ExplicitPeerList_ClearsTable(t *testing.T) {
	b := newCleanupTestBuilder(int32Ptr(3))

	d := cleanupDesired(b, false)

	if len(d.ProxySQLServers) != 0 {
		t.Errorf("ProxySQLServers = %v, want empty: an explicit spec.proxysqlServers list must be cleared on deletion, not replaced with derived in-cluster peers", d.ProxySQLServers)
	}
}

// Every other managed table must still be cleared on cleanup — only
// proxysql_servers gets the preserved-peers treatment. Enumerates every
// Desired field except ProxySQLServers (covered above) and the variables
// maps (deliberately left untouched on cleanup: ProxySQL has no "unset").
func TestCleanupDesired_ClearsEverythingElse(t *testing.T) {
	b := newCleanupTestBuilder(int32Ptr(3))

	d := cleanupDesired(b, true)

	if len(d.MySQLServers) != 0 {
		t.Errorf("MySQLServers = %v, want empty", d.MySQLServers)
	}
	if len(d.MySQLUsers) != 0 {
		t.Errorf("MySQLUsers = %v, want empty", d.MySQLUsers)
	}
	if len(d.MySQLQueryRules) != 0 {
		t.Errorf("MySQLQueryRules = %v, want empty", d.MySQLQueryRules)
	}
	if len(d.MySQLReplicationHostgroups) != 0 {
		t.Errorf("MySQLReplicationHostgroups = %v, want empty", d.MySQLReplicationHostgroups)
	}
	if len(d.MySQLHostgroupAttributes) != 0 {
		t.Errorf("MySQLHostgroupAttributes = %v, want empty", d.MySQLHostgroupAttributes)
	}
	if len(d.PostgreSQLServers) != 0 {
		t.Errorf("PostgreSQLServers = %v, want empty", d.PostgreSQLServers)
	}
	if len(d.PostgreSQLUsers) != 0 {
		t.Errorf("PostgreSQLUsers = %v, want empty", d.PostgreSQLUsers)
	}
	if len(d.PostgreSQLQueryRules) != 0 {
		t.Errorf("PostgreSQLQueryRules = %v, want empty", d.PostgreSQLQueryRules)
	}
	if len(d.SQLStatements) != 0 {
		t.Errorf("SQLStatements = %v, want empty", d.SQLStatements)
	}
}

// autoPopulatedProxySQLServers is the single shared peer derivation; the deletion path
// (cleanupDesired, auto-populated case) and the sync path (buildUnionedDesired) both consume
// it, so they must derive the exact same peer rows from the same builder — the #956 union
// must not fork the derivation logic.
func TestAutoPopulatedProxySQLServers_MatchesCleanupDesired(t *testing.T) {
	b := newCleanupTestBuilder(int32Ptr(3))
	built := autoPopulatedProxySQLServers(b)
	cleanup := cleanupDesired(b, true)

	if !reflect.DeepEqual(built, cleanup.ProxySQLServers) {
		t.Errorf("autoPopulatedProxySQLServers = %v, cleanupDesired.ProxySQLServers = %v, want equal",
			built, cleanup.ProxySQLServers)
	}
}

// The peer list the operator PUSHES must be the same one the bootstrap cnf
// SEEDS. In coreSatellite mode that is the core pods — never `<cluster>-N`.
//
// This is the regression for the first ProxySQLConfig apply destroying the
// core peer list: autoPopulatedProxySQLServers used to derive the direct-mode
// names from spec.replicas, which the CRD defaults to 3 even when the user
// omits it (as every coreSatellite cluster does). syncProxySQLServers DELETEs
// the table before inserting, so a single apply replaced the real cores with
// three pods that do not exist — the bare StatefulSet is pruned at conversion
// — and cluster_proxysql_servers_save_to_disk made it permanent.
func TestAutoPopulatedProxySQLServers_CoreSatellite_UsesCorePods(t *testing.T) {
	c := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-test", Namespace: "default"},
		Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
			// replicas deliberately omitted: DefaultedSpec fills in 3.
			Topology: &proxysqlv1alpha1.TopologySpec{
				Mode: proxysqlv1alpha1.TopologyModeCoreSatellite,
				Core: proxysqlv1alpha1.CoreSpec{Zones: []proxysqlv1alpha1.CoreZone{
					{Zone: "us-east-1a", Replicas: 2},
					{Zone: "us-east-1b", Replicas: 1},
				}},
				Satellites: proxysqlv1alpha1.SatellitesSpec{Replicas: 6},
			},
		},
	}
	b := builders.New(c, nil, defaultPw)
	if b.Spec.Replicas == nil || *b.Spec.Replicas != 3 {
		t.Fatalf("test setup: defaulted spec.replicas = %v, want the CRD default 3 so the bug is reachable", b.Spec.Replicas)
	}

	got := autoPopulatedProxySQLServers(b)

	want := []string{
		"cleanup-test-core-us-east-1a-0.cleanup-test-headless.default.svc",
		"cleanup-test-core-us-east-1a-1.cleanup-test-headless.default.svc",
		"cleanup-test-core-us-east-1b-0.cleanup-test-headless.default.svc",
	}
	if len(got) != len(want) {
		t.Fatalf("autoPopulatedProxySQLServers = %v, want the %d core pods %v", got, len(want), want)
	}
	for i, host := range want {
		if got[i].Hostname != host {
			t.Errorf("ProxySQLServers[%d].Hostname = %q, want %q", i, got[i].Hostname, host)
		}
		if got[i].Port != b.Spec.Protocols.Admin.Port {
			t.Errorf("ProxySQLServers[%d].Port = %d, want %d", i, got[i].Port, b.Spec.Protocols.Admin.Port)
		}
		if got[i].Comment != autoPopulatedPeerComment {
			t.Errorf("ProxySQLServers[%d].Comment = %q, want %q", i, got[i].Comment, autoPopulatedPeerComment)
		}
	}
	for _, g := range got {
		if strings.HasPrefix(g.Hostname, "cleanup-test-0.") ||
			strings.HasPrefix(g.Hostname, "cleanup-test-1.") ||
			strings.HasPrefix(g.Hostname, "cleanup-test-2.") {
			t.Errorf("peer %q is a direct-mode `<cluster>-N` name: that pod does not exist in coreSatellite mode", g.Hostname)
		}
		if strings.Contains(g.Hostname, "-satellite-") {
			t.Errorf("peer %q is a satellite: no core may ever sync from a satellite", g.Hostname)
		}
	}
}

// cleanupDesired shares the derivation, so a coreSatellite cluster keeps its
// core peers when a ProxySQLConfig is deleted.
func TestCleanupDesired_CoreSatellite_PreservesCorePeers(t *testing.T) {
	c := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-test", Namespace: "default"},
		Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
			Topology: &proxysqlv1alpha1.TopologySpec{
				Mode: proxysqlv1alpha1.TopologyModeCoreSatellite,
				Core: proxysqlv1alpha1.CoreSpec{Zones: []proxysqlv1alpha1.CoreZone{
					{Zone: "us-east-1a", Replicas: 1},
				}},
				Satellites: proxysqlv1alpha1.SatellitesSpec{Replicas: 2},
			},
		},
	}
	b := builders.New(c, nil, defaultPw)

	d := cleanupDesired(b, true)

	want := "cleanup-test-core-us-east-1a-0.cleanup-test-headless.default.svc"
	if len(d.ProxySQLServers) != 1 || d.ProxySQLServers[0].Hostname != want {
		t.Errorf("ProxySQLServers = %v, want the single core pod %q", d.ProxySQLServers, want)
	}
}
