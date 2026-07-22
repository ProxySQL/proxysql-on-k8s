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
	"testing"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

func TestParseCnfVariables_FromRenderedTemplate(t *testing.T) {
	c := newCluster("vars")
	b := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	b.Spec.Variables = proxysqlv1alpha1.VariablesSpec{
		MySQL: map[string]string{"mysql-max_connections": "700"},
		Admin: map[string]string{"admin-refresh_interval": "2500"},
	}
	cnf, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	got := ParseCnfVariables(cnf)

	if got["mysql-max_connections"] != "700" {
		t.Errorf("mysql-max_connections: got %q, want %q", got["mysql-max_connections"], "700")
	}
	if got["admin-refresh_interval"] != "2500" {
		t.Errorf("admin-refresh_interval: got %q, want %q", got["admin-refresh_interval"], "2500")
	}
	if _, ok := got["admin-admin_credentials"]; ok {
		t.Errorf("admin-admin_credentials must be excluded (reserved), got %v", got["admin-admin_credentials"])
	}
	if _, ok := got["mysql-interfaces"]; ok {
		t.Errorf("mysql-interfaces must be excluded (reserved), got %v", got["mysql-interfaces"])
	}
}

func TestNormalizeCnf_VariablesOnlyChangeIsInvariant(t *testing.T) {
	c := newCluster("norm-vars")
	b := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	b.Spec.Variables = proxysqlv1alpha1.VariablesSpec{
		MySQL: map[string]string{"mysql-max_connections": "700"},
	}
	cnfA, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	b2 := New(newCluster("norm-vars"), newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	b2.Spec.Variables = proxysqlv1alpha1.VariablesSpec{
		MySQL: map[string]string{"mysql-max_connections": "1500"},
	}
	cnfB, err := b2.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	if cnfA == cnfB {
		t.Fatal("test setup broken: renders should differ before normalization")
	}
	if NormalizeCnf(cnfA) != NormalizeCnf(cnfB) {
		t.Errorf("normalized cnf should be equal for a variables-only value change:\nA:\n%s\nB:\n%s", NormalizeCnf(cnfA), NormalizeCnf(cnfB))
	}
}

func TestNormalizeCnf_StructuralChangeDiffers(t *testing.T) {
	// Differing replicas -> different proxysql_servers block (structural).
	c1 := newCluster("norm-struct", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Replicas = int32Ptr(3)
	})
	b1 := New(c1, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	cnf1, err := b1.BootstrapCnf(b1.ProxySQLServerDNS())
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	c2 := newCluster("norm-struct", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Replicas = int32Ptr(5)
	})
	b2 := New(c2, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	cnf2, err := b2.BootstrapCnf(b2.ProxySQLServerDNS())
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	if NormalizeCnf(cnf1) == NormalizeCnf(cnf2) {
		t.Errorf("normalized cnf should differ when the proxysql_servers block (structural) differs")
	}

	// Differing credentials -> different admin_credentials line (reserved,
	// so left verbatim by NormalizeCnf).
	bCred1 := New(newCluster("cred"), newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	cnfCred1, err := bCred1.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	bCred2 := New(newCluster("cred"), newScheme(t), Passwords{Admin: "different", Radmin: "r", Monitor: "m"})
	cnfCred2, err := bCred2.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	if NormalizeCnf(cnfCred1) == NormalizeCnf(cnfCred2) {
		t.Errorf("normalized cnf should differ when reserved admin_credentials differ")
	}
}

func TestNormalizeCnf_RemovedVariableDiffers(t *testing.T) {
	c := newCluster("norm-removed")
	bWith := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	bWith.Spec.Variables = proxysqlv1alpha1.VariablesSpec{
		MySQL: map[string]string{"mysql-max_connections": "700"},
	}
	cnfWith, err := bWith.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	bWithout := New(newCluster("norm-removed"), newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	cnfWithout, err := bWithout.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}

	if NormalizeCnf(cnfWith) == NormalizeCnf(cnfWithout) {
		t.Errorf("normalized cnf should differ when a variable line is removed entirely")
	}
}
