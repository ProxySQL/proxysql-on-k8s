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
	"context"
	"strings"
	"testing"
)

func ptr32(v int32) *int32 { return &v }

// fakeQuerier returns canned rows keyed by a substring of the query.
type fakeQuerier struct {
	rows map[string][][]string
}

func (f *fakeQuerier) Query(_ context.Context, q string) ([][]string, error) {
	for k, v := range f.rows {
		// Suffix match (table name ends the FROM clause) keeps lookups
		// unambiguous even if a future key is a prefix of another.
		if strings.HasSuffix(q, k) {
			return v, nil
		}
	}
	return nil, nil
}

func TestReadRuntime_ParsesTables(t *testing.T) {
	fq := &fakeQuerier{rows: map[string][][]string{
		"runtime_mysql_servers": {
			{"0", "db-0", "3306", "ONLINE"},
			{"1", "db-1", "3306", "SHUNNED"},
		},
		"runtime_mysql_users":       {{"app"}},
		"runtime_mysql_query_rules": {{"1"}},
		"runtime_pgsql_servers":     {},
		"runtime_pgsql_users":       {},
		"runtime_pgsql_query_rules": {},
	}}
	rs, err := ReadRuntime(context.Background(), fq)
	if err != nil {
		t.Fatalf("ReadRuntime: %v", err)
	}
	if got := rs.MySQLServers["0:db-0:3306"]; got != "ONLINE" {
		t.Errorf("server 0:db-0:3306 status = %q, want ONLINE", got)
	}
	if rs.ShunnedCount() != 1 {
		t.Errorf("ShunnedCount = %d, want 1", rs.ShunnedCount())
	}
	if !rs.MySQLUsers["app"] {
		t.Errorf("user app missing from runtime state")
	}
	if !rs.MySQLRules["1"] {
		t.Errorf("rule 1 missing from runtime state")
	}
}

func TestDrift_NoDriftWhenConverged(t *testing.T) {
	d := &Desired{
		MySQLServers:    []MySQLServer{{Hostgroup: 0, Hostname: "db-0", Port: 3306}},
		MySQLUsers:      []MySQLUser{{Username: "app", Password: "x"}},
		MySQLQueryRules: []MySQLQueryRule{{RuleID: 1, MatchDigest: "^SELECT", DestinationHostgroup: ptr32(1)}},
	}
	rs := &RuntimeState{
		MySQLServers: map[string]string{"0:db-0:3306": "ONLINE"},
		MySQLUsers:   map[string]bool{"app": true},
		MySQLRules:   map[string]bool{"1": true},
	}
	if diffs := d.Drift(rs); len(diffs) != 0 {
		t.Errorf("Drift = %v, want none", diffs)
	}
}

func TestDrift_DetectsMissingAndExtra(t *testing.T) {
	d := &Desired{
		MySQLServers: []MySQLServer{{Hostgroup: 0, Hostname: "db-0"}}, // Port 0 => defaults to 3306
	}
	rs := &RuntimeState{
		MySQLServers: map[string]string{"0:stale-host:3306": "ONLINE"},
		MySQLUsers:   map[string]bool{"ghost": true},
	}
	diffs := d.Drift(rs)
	want := []string{
		"mysql_servers: extra 0:stale-host:3306",
		"mysql_servers: missing 0:db-0:3306",
		"mysql_users: extra ghost",
	}
	if len(diffs) != len(want) {
		t.Fatalf("Drift = %v, want %v", diffs, want)
	}
	for i := range want {
		if diffs[i] != want[i] {
			t.Errorf("Drift[%d] = %q, want %q", i, diffs[i], want[i])
		}
	}
}

// pairedDesired is the canonical replication-hostgroup fixture: every node
// listed in the writer hostgroup (10), pair (10,20) — the topology the docs
// recommend for read_only-monitored backends.
func pairedDesired() *Desired {
	return &Desired{
		MySQLServers: []MySQLServer{
			{Hostgroup: 10, Hostname: "db-0", Port: 3306},
			{Hostgroup: 10, Hostname: "db-1", Port: 3306},
			{Hostgroup: 10, Hostname: "db-2", Port: 3306},
		},
		MySQLReplicationHostgroups: []MySQLReplicationHostgroup{
			{WriterHostgroup: 10, ReaderHostgroup: 20, CheckType: "read_only"},
		},
	}
}

// The monitor demoting/promoting servers between the hostgroups of a
// mysql_replication_hostgroups pair is ProxySQL doing its job, not drift.
// Membership is what the operator owns; placement within the pair is not.
func TestDrift_MonitorMoveWithinPairIsNotDrift(t *testing.T) {
	d := pairedDesired()
	rs := &RuntimeState{
		MySQLServers: map[string]string{
			"10:db-0:3306": "ONLINE",  // stayed a writer
			"20:db-1:3306": "ONLINE",  // demoted to reader by read_only=1
			"20:db-2:3306": "SHUNNED", // demoted AND shunned — still present
		},
	}
	if diffs := d.Drift(rs); len(diffs) != 0 {
		t.Errorf("Drift = %v, want none (moves within the pair are monitor placement)", diffs)
	}
}

// writer_is_also_reader keeps the writer in BOTH hostgroups of the pair at
// runtime. Two rows for one member must not read as an extra server.
func TestDrift_WriterAlsoReaderIsNotDrift(t *testing.T) {
	d := pairedDesired()
	rs := &RuntimeState{
		MySQLServers: map[string]string{
			"10:db-0:3306": "ONLINE",
			"20:db-0:3306": "ONLINE", // same server mirrored into the reader hostgroup
			"20:db-1:3306": "ONLINE",
			"20:db-2:3306": "ONLINE",
		},
	}
	if diffs := d.Drift(rs); len(diffs) != 0 {
		t.Errorf("Drift = %v, want none (writer_is_also_reader duplicates one member)", diffs)
	}
}

// A member gone from BOTH hostgroups of the pair is real drift; the message
// names the spec placement.
func TestDrift_MissingMemberWithPairIsDrift(t *testing.T) {
	d := pairedDesired()
	rs := &RuntimeState{
		MySQLServers: map[string]string{
			"10:db-0:3306": "ONLINE",
			"20:db-1:3306": "ONLINE",
			// db-2 wiped out-of-band
		},
	}
	diffs := d.Drift(rs)
	want := []string{"mysql_servers: missing 10:db-2:3306"}
	if len(diffs) != 1 || diffs[0] != want[0] {
		t.Errorf("Drift = %v, want %v", diffs, want)
	}
}

// An unknown server inside a paired hostgroup is real drift; the message
// names the runtime row.
func TestDrift_ExtraServerWithPairIsDrift(t *testing.T) {
	d := pairedDesired()
	rs := &RuntimeState{
		MySQLServers: map[string]string{
			"10:db-0:3306":  "ONLINE",
			"20:db-1:3306":  "ONLINE",
			"20:db-2:3306":  "ONLINE",
			"20:ghost:3306": "ONLINE",
		},
	}
	diffs := d.Drift(rs)
	want := []string{"mysql_servers: extra 20:ghost:3306"}
	if len(diffs) != 1 || diffs[0] != want[0] {
		t.Errorf("Drift = %v, want %v", diffs, want)
	}
}

// The same member on a different PORT is a different backend — port is
// identity, not placement.
func TestDrift_PortChangeWithPairIsDrift(t *testing.T) {
	d := pairedDesired()
	rs := &RuntimeState{
		MySQLServers: map[string]string{
			"10:db-0:3307": "ONLINE", // wrong port
			"20:db-1:3306": "ONLINE",
			"20:db-2:3306": "ONLINE",
		},
	}
	diffs := d.Drift(rs)
	want := []string{
		"mysql_servers: extra 10:db-0:3307",
		"mysql_servers: missing 10:db-0:3306",
	}
	if len(diffs) != len(want) {
		t.Fatalf("Drift = %v, want %v", diffs, want)
	}
	for i := range want {
		if diffs[i] != want[i] {
			t.Errorf("Drift[%d] = %q, want %q", i, diffs[i], want[i])
		}
	}
}

// Hostgroups not covered by any pair keep exact-placement semantics even
// when pairs exist for other hostgroups: with no pair there is no legitimate
// mover, so a hostgroup change there is drift.
func TestDrift_UnpairedHostgroupKeepsExactPlacement(t *testing.T) {
	d := pairedDesired()
	d.MySQLServers = append(d.MySQLServers, MySQLServer{Hostgroup: 30, Hostname: "analytics", Port: 3306})
	rs := &RuntimeState{
		MySQLServers: map[string]string{
			"10:db-0:3306":      "ONLINE",
			"10:db-1:3306":      "ONLINE",
			"10:db-2:3306":      "ONLINE",
			"31:analytics:3306": "ONLINE", // moved out of hg 30 — nothing may do that
		},
	}
	diffs := d.Drift(rs)
	want := []string{
		"mysql_servers: extra 31:analytics:3306",
		"mysql_servers: missing 30:analytics:3306",
	}
	if len(diffs) != len(want) {
		t.Fatalf("Drift = %v, want %v", diffs, want)
	}
	for i := range want {
		if diffs[i] != want[i] {
			t.Errorf("Drift[%d] = %q, want %q", i, diffs[i], want[i])
		}
	}
}

// Two disjoint pairs are independent equivalence classes: a server may move
// within its own pair, never across pairs.
func TestDrift_DisjointPairsDoNotMerge(t *testing.T) {
	d := &Desired{
		MySQLServers: []MySQLServer{
			{Hostgroup: 10, Hostname: "a", Port: 3306},
			{Hostgroup: 30, Hostname: "b", Port: 3306},
		},
		MySQLReplicationHostgroups: []MySQLReplicationHostgroup{
			{WriterHostgroup: 10, ReaderHostgroup: 20},
			{WriterHostgroup: 30, ReaderHostgroup: 40},
		},
	}
	// "a" moved into the OTHER pair's hostgroup — that's drift.
	rs := &RuntimeState{
		MySQLServers: map[string]string{
			"40:a:3306": "ONLINE",
			"30:b:3306": "ONLINE",
		},
	}
	diffs := d.Drift(rs)
	want := []string{
		"mysql_servers: extra 40:a:3306",
		"mysql_servers: missing 10:a:3306",
	}
	if len(diffs) != len(want) {
		t.Fatalf("Drift = %v, want %v", diffs, want)
	}
	for i := range want {
		if diffs[i] != want[i] {
			t.Errorf("Drift[%d] = %q, want %q", i, diffs[i], want[i])
		}
	}
	// ...but a move within its own pair is not.
	rs = &RuntimeState{
		MySQLServers: map[string]string{
			"20:a:3306": "ONLINE",
			"40:b:3306": "ONLINE",
		},
	}
	if diffs := d.Drift(rs); len(diffs) != 0 {
		t.Errorf("Drift = %v, want none (each server moved within its own pair)", diffs)
	}
}

// Pairs sharing a hostgroup chain into one equivalence class — ProxySQL
// itself treats all hostgroups reachable through shared pairs as one
// replication topology, so membership anywhere in the chain counts.
func TestDrift_OverlappingPairsChain(t *testing.T) {
	d := &Desired{
		MySQLServers: []MySQLServer{{Hostgroup: 10, Hostname: "a", Port: 3306}},
		MySQLReplicationHostgroups: []MySQLReplicationHostgroup{
			{WriterHostgroup: 10, ReaderHostgroup: 20},
			{WriterHostgroup: 20, ReaderHostgroup: 30},
		},
	}
	rs := &RuntimeState{
		MySQLServers: map[string]string{"30:a:3306": "ONLINE"},
	}
	if diffs := d.Drift(rs); len(diffs) != 0 {
		t.Errorf("Drift = %v, want none (hg 30 chains to the pair via hg 20)", diffs)
	}
}

// pgsql servers have no replication-hostgroup concept in this operator
// (Desired carries no PostgreSQL pairs), so exact placement stays enforced
// for them — MySQL pairs must not loosen the pgsql comparison.
func TestDrift_PgSQLKeepsExactPlacement(t *testing.T) {
	d := pairedDesired()
	d.PostgreSQLServers = []PostgreSQLServer{{Hostgroup: 10, Hostname: "pg-0", Port: 5432}}
	rs := &RuntimeState{
		MySQLServers: map[string]string{
			"10:db-0:3306": "ONLINE",
			"10:db-1:3306": "ONLINE",
			"10:db-2:3306": "ONLINE",
		},
		PgSQLServers: map[string]string{"20:pg-0:5432": "ONLINE"},
	}
	diffs := d.Drift(rs)
	want := []string{
		"pgsql_servers: extra 20:pg-0:5432",
		"pgsql_servers: missing 10:pg-0:5432",
	}
	if len(diffs) != len(want) {
		t.Fatalf("Drift = %v, want %v", diffs, want)
	}
	for i := range want {
		if diffs[i] != want[i] {
			t.Errorf("Drift[%d] = %q, want %q", i, diffs[i], want[i])
		}
	}
}

type recordingQuerier struct {
	query string
}

func (r *recordingQuerier) Query(_ context.Context, q string) ([][]string, error) {
	r.query = q
	return [][]string{
		{"mysql-max_connections", "2000"},
		{"mysql-max_allowed_packet", "16777216"},
	}, nil
}

func TestReadGlobalVariables_FiltersAndMaps(t *testing.T) {
	rq := &recordingQuerier{}
	result, err := ReadGlobalVariables(context.Background(), rq, []string{"mysql-max_connections", "mysql-max_allowed_packet"})
	if err != nil {
		t.Fatalf("ReadGlobalVariables: %v", err)
	}
	if got := result["mysql-max_connections"]; got != "2000" {
		t.Errorf("mysql-max_connections = %q, want 2000", got)
	}
	if got := result["mysql-max_allowed_packet"]; got != "16777216" {
		t.Errorf("mysql-max_allowed_packet = %q, want 16777216", got)
	}
	// Verify the query contains both quoted names
	if !strings.Contains(rq.query, "'mysql-max_connections'") {
		t.Errorf("query missing 'mysql-max_connections' in quoted form: %q", rq.query)
	}
	if !strings.Contains(rq.query, "'mysql-max_allowed_packet'") {
		t.Errorf("query missing 'mysql-max_allowed_packet' in quoted form: %q", rq.query)
	}
}

func TestReadGlobalVariables_EmptyNamesNoQuery(t *testing.T) {
	fq := &fakeQuerier{
		rows: map[string][][]string{},
	}
	result, err := ReadGlobalVariables(context.Background(), fq, []string{})
	if err != nil {
		t.Fatalf("ReadGlobalVariables: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("result = %v, want empty map", result)
	}
}
