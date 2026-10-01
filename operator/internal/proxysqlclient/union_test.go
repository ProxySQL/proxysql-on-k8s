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

package proxysqlclient

import (
	"reflect"
	"testing"
)

// TestUnion_DistinctKeys_Combines verifies that entries with different keys from
// separate configs all survive the union (the whole point of #956 — two configs
// composing one runtime instead of wiping each other).
func TestUnion_DistinctKeys_Combines(t *testing.T) {
	a := &Desired{
		MySQLServers: []MySQLServer{{Hostgroup: 10, Hostname: "w", Port: 3306}},
		MySQLUsers:   []MySQLUser{{Username: "app"}},
	}
	b := &Desired{
		MySQLServers:    []MySQLServer{{Hostgroup: 20, Hostname: "r", Port: 3306}},
		MySQLUsers:      []MySQLUser{{Username: "reporting"}},
		MySQLQueryRules: []MySQLQueryRule{{RuleID: 100}},
	}
	got := Union([]*Desired{a, b})
	if len(got.MySQLServers) != 2 {
		t.Fatalf("want 2 servers (both configs), got %d: %+v", len(got.MySQLServers), got.MySQLServers)
	}
	if len(got.MySQLUsers) != 2 {
		t.Fatalf("want 2 users, got %d", len(got.MySQLUsers))
	}
	if len(got.MySQLQueryRules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(got.MySQLQueryRules))
	}
	// Sorted output: hostgroup 10 before 20.
	if got.MySQLServers[0].Hostgroup != 10 || got.MySQLServers[1].Hostgroup != 20 {
		t.Errorf("servers not sorted by key: %+v", got.MySQLServers)
	}
}

// TestUnion_SameKey_LastWriterWins verifies that when two configs collide on a key,
// the later element (caller sorts by config name) overrides the earlier one.
func TestUnion_SameKey_LastWriterWins(t *testing.T) {
	first := &Desired{
		MySQLServers:   []MySQLServer{{Hostgroup: 10, Hostname: "h", Port: 3306, Comment: "first"}},
		MySQLVariables: map[string]string{"mysql-max_connections": "100"},
	}
	last := &Desired{
		MySQLServers:   []MySQLServer{{Hostgroup: 10, Hostname: "h", Port: 3306, Comment: "last"}},
		MySQLVariables: map[string]string{"mysql-max_connections": "500"},
	}
	got := Union([]*Desired{first, last})
	if len(got.MySQLServers) != 1 {
		t.Fatalf("same key must collapse to 1 server, got %d", len(got.MySQLServers))
	}
	if got.MySQLServers[0].Comment != "last" {
		t.Errorf("last-writer-wins failed for server: got comment %q", got.MySQLServers[0].Comment)
	}
	if got.MySQLVariables["mysql-max_connections"] != "500" {
		t.Errorf("last-writer-wins failed for variable: got %q", got.MySQLVariables["mysql-max_connections"])
	}
}

// Galera hostgroups merge by writer hostgroup, like replication hostgroups:
// distinct rows from separate configs combine, and a collision on the writer
// hostgroup resolves last-writer-wins.
func TestUnion_GaleraHostgroups_MergeByWriterHostgroup(t *testing.T) {
	a := &Desired{MySQLGaleraHostgroups: []MySQLGaleraHostgroup{{
		WriterHostgroup: 10, BackupWriterHostgroup: 12, ReaderHostgroup: 11, OfflineHostgroup: 13,
	}}}
	b := &Desired{MySQLGaleraHostgroups: []MySQLGaleraHostgroup{{
		WriterHostgroup: 20, BackupWriterHostgroup: 22, ReaderHostgroup: 21, OfflineHostgroup: 23,
	}}}
	got := Union([]*Desired{b, a}) // reversed input: output must still be key-sorted
	if len(got.MySQLGaleraHostgroups) != 2 {
		t.Fatalf("want 2 galera rows, got %d: %+v", len(got.MySQLGaleraHostgroups), got.MySQLGaleraHostgroups)
	}
	if got.MySQLGaleraHostgroups[0].WriterHostgroup != 10 || got.MySQLGaleraHostgroups[1].WriterHostgroup != 20 {
		t.Errorf("galera rows not sorted by writer hostgroup: %+v", got.MySQLGaleraHostgroups)
	}

	first := &Desired{MySQLGaleraHostgroups: []MySQLGaleraHostgroup{{
		WriterHostgroup: 10, BackupWriterHostgroup: 12, ReaderHostgroup: 11,
		OfflineHostgroup: 13, Comment: "first",
	}}}
	last := &Desired{MySQLGaleraHostgroups: []MySQLGaleraHostgroup{{
		WriterHostgroup: 10, BackupWriterHostgroup: 12, ReaderHostgroup: 11,
		OfflineHostgroup: 13, Comment: "last",
	}}}
	got = Union([]*Desired{first, last})
	if len(got.MySQLGaleraHostgroups) != 1 {
		t.Fatalf("same writer hostgroup must collapse to 1 row, got %d", len(got.MySQLGaleraHostgroups))
	}
	if got.MySQLGaleraHostgroups[0].Comment != "last" {
		t.Errorf("last-writer-wins failed: got comment %q", got.MySQLGaleraHostgroups[0].Comment)
	}

	// No galera rows anywhere in the input must stay nil, not an empty slice.
	if got := Union([]*Desired{{}}); got.MySQLGaleraHostgroups != nil {
		t.Errorf("empty galera section must stay nil, got %v", got.MySQLGaleraHostgroups)
	}
}

// TestUnion_Deterministic verifies the union is stable regardless of nothing-changed
// re-runs (a flapping fingerprint would cause needless re-pushes / connection drops).
func TestUnion_Deterministic(t *testing.T) {
	a := &Desired{MySQLServers: []MySQLServer{{Hostgroup: 30, Hostname: "c", Port: 3306}}}
	b := &Desired{MySQLServers: []MySQLServer{{Hostgroup: 10, Hostname: "a", Port: 3306}, {Hostgroup: 20, Hostname: "b", Port: 3306}}}
	first := Union([]*Desired{a, b})
	second := Union([]*Desired{a, b})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Union not deterministic:\n%+v\n%+v", first, second)
	}
}

// TestUnion_SQLStatements_ConcatInOrder + empty sections stay nil.
func TestUnion_SQLStatements_And_EmptyNil(t *testing.T) {
	a := &Desired{SQLStatements: []string{"SET a=1"}}
	b := &Desired{SQLStatements: []string{"SET b=2"}}
	got := Union([]*Desired{a, b})
	if !reflect.DeepEqual(got.SQLStatements, []string{"SET a=1", "SET b=2"}) {
		t.Errorf("sqlStatements not concatenated in order: %v", got.SQLStatements)
	}
	if got.MySQLServers != nil || got.MySQLVariables != nil {
		t.Errorf("empty sections must stay nil, got servers=%v vars=%v", got.MySQLServers, got.MySQLVariables)
	}
}
