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

package builders

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// TestGolden is the upgrade-stability gate (issue #58): it pins the
// rendered bootstrap cnf and pod template for a FIXED reference
// ProxySQLCluster spec against committed golden files, byte-for-byte.
//
// The builders in this package are pure functions of (spec, passwords) —
// see CLAUDE.md's "Builders are pure" convention — so for an unchanged
// spec their output must never change across operator versions. If it
// does, every existing cluster gets a one-time rolling restart on the
// next reconcile after upgrade (the cnf-checksum annotation or the pod
// template itself changed). That may be an intentional, justified change
// (a new default, a security-context tightening, a bugfix) — but it must
// be a CONSCIOUS commit, not an accidental side effect of an unrelated
// refactor, and it requires a release-note entry describing the restart.
//
// See CLAUDE.md's "Upgrade stability" section for the policy this test
// enforces.
const goldenDir = "testdata/golden"

// goldenPasswords are fixed, non-secret placeholder credentials used only
// so the golden cnf's admin_credentials/monitor_password lines render
// deterministically across runs and machines. They are test fixtures, not
// real passwords used anywhere else.
var goldenPasswords = Passwords{
	Admin:   "golden-admin-pw",
	Radmin:  "golden-radmin-pw",
	Monitor: "golden-monitor-pw",
}

// goldenCluster returns the fixed reference ProxySQLCluster pinned by
// TestGolden: 3 replicas (so ProxySQL Cluster sync and its
// proxysql_servers/cluster_check_* wiring are exercised), MySQL and
// PostgreSQL enabled, metrics and the web UI on, persistence on, one
// spec.variables entry per domain (admin/mysql/pgsql), logging disabled.
//
// Do not "improve" this spec to track new features — it is a snapshot.
// Any deliberate expansion of what it covers must come with regenerated
// goldens and a matching release-note entry (see TestGolden and
// CLAUDE.md's "Upgrade stability" section).
func goldenCluster() *proxysqlv1alpha1.ProxySQLCluster {
	replicas := int32(3)
	return &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden",
			Namespace: "golden-ns",
		},
		Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
			Replicas: &replicas,
			Protocols: proxysqlv1alpha1.ProtocolsSpec{
				MySQL:      proxysqlv1alpha1.ProtocolSpec{Enabled: boolPtr(true)},
				PostgreSQL: proxysqlv1alpha1.ProtocolSpec{Enabled: boolPtr(true)},
				Web:        proxysqlv1alpha1.ProtocolSpec{Enabled: boolPtr(true)},
			},
			Metrics:     proxysqlv1alpha1.MetricsSpec{Enabled: boolPtr(true)},
			Persistence: proxysqlv1alpha1.PersistenceSpec{Enabled: boolPtr(true)},
			Variables: proxysqlv1alpha1.VariablesSpec{
				Admin:      map[string]string{"admin-cluster_check_interval_ms": "300"},
				MySQL:      map[string]string{"mysql-max_connections": "1000"},
				PostgreSQL: map[string]string{"pgsql-max_connections": "500"},
			},
			// Logging left nil: the sidecar defaults to off (LoggingEnabled
			// requires an explicit spec.logging.enabled=true).
		},
	}
}

// TestGolden renders the bootstrap cnf and the StatefulSet pod template for
// goldenCluster() and compares them byte-for-byte against the committed
// files under testdata/golden/. Run with UPDATE_GOLDEN=1 to (re)write them:
//
//	UPDATE_GOLDEN=1 go test ./internal/controller/builders/ -run TestGolden
//
// Regenerating and committing a changed golden is a statement that the
// output change is intentional and ships with a release note — see
// CLAUDE.md's "Upgrade stability" section and issue #58.
func TestGolden(t *testing.T) {
	cluster := goldenCluster()
	b := New(cluster, newScheme(t), goldenPasswords)

	proxysqlServers := b.ProxySQLServerDNS()

	cnf, err := b.BootstrapCnf(proxysqlServers)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	// Mirror the reconciler's real path (proxysqlcluster_controller.go):
	// the pod-template's proxysql.com/cnf-checksum annotation is
	// CnfChecksum(cnfSecret.Data), not an arbitrary test value — so the
	// golden pod template reflects the same checksum a live cluster would
	// actually receive for this spec.
	cnfSecret, err := b.CnfSecret()
	if err != nil {
		t.Fatalf("CnfSecret: %v", err)
	}
	checksum := CnfChecksum(cnfSecret.Data)

	sts := b.StatefulSet(checksum)
	podTemplateYAML, err := yaml.Marshal(sts.Spec.Template)
	if err != nil {
		t.Fatalf("marshal pod template: %v", err)
	}

	checkGolden(t, "bootstrap.cnf", []byte(cnf))
	checkGolden(t, "pod-template.yaml", podTemplateYAML)
}

// checkGolden compares got against testdata/golden/<name>. With
// UPDATE_GOLDEN=1 set, it (re)writes the file instead of comparing.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(goldenDir, name)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", goldenDir, err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("wrote golden %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf(
			"read golden %s: %v\nregenerate with `UPDATE_GOLDEN=1 go test ./internal/controller/builders/ -run TestGolden`",
			path, err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("%s", goldenMismatchMsg(name, path, want, got))
	}
}

// goldenMismatchMsg builds the mandatory failure text for a golden
// mismatch: how to regenerate, and what a diff here actually means for
// operators (see CLAUDE.md's "Upgrade stability" section, issue #58).
func goldenMismatchMsg(name, path string, want, got []byte) string {
	return fmt.Sprintf(`golden mismatch for %s (%s)

regenerate with: UPDATE_GOLDEN=1 go test ./internal/controller/builders/ -run TestGolden

This diff means the rendered cnf or pod template changed for an UNCHANGED
ProxySQLCluster spec: on the next operator upgrade, every existing cluster's
ProxySQL pods will get a one-time rolling restart (the pod template and/or
the proxysql.com/cnf-checksum annotation differ from what this version would
have produced). Per CLAUDE.md's "Upgrade stability" policy (issue #58), that
restart must be a conscious, reviewed decision — regenerating this golden
and committing the diff IS the release-note item; it is not optional
cleanup.

--- want (%s)
+++ got
%s
%s
`, name, path, path, want, got)
}
