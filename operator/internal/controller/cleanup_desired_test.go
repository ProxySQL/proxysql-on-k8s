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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

	d := cleanupDesired(b)

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

	d := cleanupDesired(b)

	if len(d.ProxySQLServers) != 0 {
		t.Errorf("ProxySQLServers = %v, want empty for a single-replica cluster", d.ProxySQLServers)
	}
}

// Every other managed table must still be cleared on cleanup — only
// proxysql_servers gets the preserved-peers treatment.
func TestCleanupDesired_ClearsEverythingElse(t *testing.T) {
	b := newCleanupTestBuilder(int32Ptr(3))

	d := cleanupDesired(b)

	if len(d.MySQLServers) != 0 {
		t.Errorf("MySQLServers = %v, want empty", d.MySQLServers)
	}
	if len(d.MySQLUsers) != 0 {
		t.Errorf("MySQLUsers = %v, want empty", d.MySQLUsers)
	}
	if len(d.MySQLQueryRules) != 0 {
		t.Errorf("MySQLQueryRules = %v, want empty", d.MySQLQueryRules)
	}
	if len(d.PostgreSQLServers) != 0 {
		t.Errorf("PostgreSQLServers = %v, want empty", d.PostgreSQLServers)
	}
	if len(d.SQLStatements) != 0 {
		t.Errorf("SQLStatements = %v, want empty", d.SQLStatements)
	}
}

// buildDesired's auto-population branch (spec.proxysqlServers empty) and
// cleanupDesired must derive the exact same peer rows from the same
// builder — the whole point of sharing autoPopulatedProxySQLServers is that
// the fix doesn't fork the derivation logic.
func TestAutoPopulatedProxySQLServers_MatchesBuildDesired(t *testing.T) {
	b := newCleanupTestBuilder(int32Ptr(3))
	r := &ProxySQLConfigReconciler{}
	cfg := &proxysqlv1alpha1.ProxySQLConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
			ClusterRef: corev1.LocalObjectReference{Name: "cleanup-test"},
		},
	}

	built, err := r.buildDesired(context.Background(), cfg, b)
	if err != nil {
		t.Fatalf("buildDesired: %v", err)
	}
	cleanup := cleanupDesired(b)

	if !reflect.DeepEqual(built.ProxySQLServers, cleanup.ProxySQLServers) {
		t.Errorf("buildDesired ProxySQLServers = %v, cleanupDesired ProxySQLServers = %v, want equal",
			built.ProxySQLServers, cleanup.ProxySQLServers)
	}
}
