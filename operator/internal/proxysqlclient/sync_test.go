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

// recorder is a fake Executor that captures every query Sync issues.
type recorder struct {
	queries []string
}

func (r *recorder) Exec(_ context.Context, q string, _ ...any) error {
	r.queries = append(r.queries, q)
	return nil
}

func (r *recorder) seen(substr string) bool {
	for _, q := range r.queries {
		if strings.Contains(q, substr) {
			return true
		}
	}
	return false
}

func TestSync_EmptyDesired_StillIssuesDeletesAndLoadSaves(t *testing.T) {
	rec := &recorder{}
	if err := Sync(context.Background(), rec, &Desired{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// An empty Desired still deletes every table (drift correction) and
	// issues LOAD/SAVE so the empty state is applied.
	mustSee := []string{
		"DELETE FROM mysql_servers",
		"DELETE FROM mysql_replication_hostgroups",
		"DELETE FROM mysql_users",
		"DELETE FROM mysql_query_rules",
		"DELETE FROM pgsql_servers",
		"DELETE FROM pgsql_users",
		"DELETE FROM pgsql_query_rules",
		"DELETE FROM proxysql_servers",
		"LOAD MYSQL SERVERS TO RUNTIME",
		"SAVE MYSQL SERVERS TO DISK",
		"LOAD MYSQL USERS TO RUNTIME",
		"LOAD MYSQL QUERY RULES TO RUNTIME",
		"LOAD PGSQL SERVERS TO RUNTIME",
		"LOAD PGSQL USERS TO RUNTIME",
		"LOAD PGSQL QUERY RULES TO RUNTIME",
		"LOAD PROXYSQL SERVERS TO RUNTIME",
	}
	for _, m := range mustSee {
		if !rec.seen(m) {
			t.Errorf("missing expected query containing %q", m)
		}
	}
	// Empty variable maps must NOT trigger their UPDATE/LOAD/SAVE cycle.
	if rec.seen("LOAD MYSQL VARIABLES") || rec.seen("LOAD PGSQL VARIABLES") || rec.seen("LOAD ADMIN VARIABLES") {
		t.Errorf("expected no variable LOAD when variable maps are empty; queries=%v", rec.queries)
	}
}

func TestSync_MySQLServers_RendersInsert(t *testing.T) {
	rec := &recorder{}
	w := int32(100)
	d := &Desired{
		MySQLServers: []MySQLServer{
			{Hostgroup: 0, Hostname: "host-a", Port: 3306, Weight: &w, Comment: "primary"},
			{Hostgroup: 1, Hostname: "host-b"}, // Port=0 → defaults to 3306
		},
	}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	var insert string
	for _, q := range rec.queries {
		if strings.HasPrefix(q, "INSERT INTO mysql_servers") {
			insert = q
			break
		}
	}
	if insert == "" {
		t.Fatalf("no INSERT into mysql_servers issued; queries=%v", rec.queries)
	}
	for _, want := range []string{
		"(0,'host-a',3306,100,",
		"(1,'host-b',3306,NULL,NULL,NULL,NULL,'')",
		"'primary'",
	} {
		if !strings.Contains(insert, want) {
			t.Errorf("INSERT missing %q\nfull: %s", want, insert)
		}
	}
}

func TestSync_PostgreSQLServers_DefaultsTo5432(t *testing.T) {
	rec := &recorder{}
	d := &Desired{PostgreSQLServers: []PostgreSQLServer{{Hostgroup: 0, Hostname: "pg"}}}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !rec.seen("(0,'pg',5432,") {
		t.Errorf("expected pgsql_servers row defaulted to port 5432; queries=%v", rec.queries)
	}
}

func TestSync_Variables_AppliedWhenSet(t *testing.T) {
	rec := &recorder{}
	d := &Desired{
		MySQLVariables: map[string]string{
			"default_query_timeout":  "36000000",
			"connect_timeout_server": "3000",
		},
	}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !rec.seen("variable_name='default_query_timeout'") {
		t.Errorf("missing UPDATE for default_query_timeout; queries=%v", rec.queries)
	}
	if !rec.seen("LOAD MYSQL VARIABLES TO RUNTIME") {
		t.Errorf("missing LOAD MYSQL VARIABLES; queries=%v", rec.queries)
	}
}

func TestSync_Quote_EscapesSingleQuotes(t *testing.T) {
	rec := &recorder{}
	d := &Desired{
		MySQLServers: []MySQLServer{{Hostgroup: 0, Hostname: "ho'st", Comment: "it's fine"}},
	}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Single quotes inside string literals must be doubled, never left raw.
	if !rec.seen("'ho''st'") || !rec.seen("'it''s fine'") {
		t.Errorf("single quotes not escaped; queries=%v", rec.queries)
	}
}

func TestSync_StepFailure_DoesNotAbortLaterSteps(t *testing.T) {
	rec := &failOnce{recorder: recorder{}, failQuery: "DELETE FROM mysql_users"}
	d := &Desired{
		MySQLServers: []MySQLServer{{Hostgroup: 0, Hostname: "h", Port: 3306}},
		MySQLUsers:   []MySQLUser{{Username: "u", Password: "p"}},
		// Force a later step (pgsql_servers) to be present so we can verify
		// it still ran after the mysql_users failure.
		PostgreSQLServers: []PostgreSQLServer{{Hostgroup: 0, Hostname: "pg"}},
	}
	err := Sync(context.Background(), rec, d)
	if err == nil {
		t.Fatalf("expected error from Sync due to forced failure, got nil")
	}
	if !strings.Contains(err.Error(), "mysql_users") {
		t.Errorf("expected error mentioning mysql_users, got %v", err)
	}
	if !rec.seen("DELETE FROM pgsql_servers") {
		t.Errorf("expected later step pgsql_servers to still run after mysql_users failure; queries=%v", rec.queries)
	}
}

type failOnce struct {
	recorder
	failQuery string
	failed    bool
}

func (f *failOnce) Exec(ctx context.Context, q string, args ...any) error {
	f.queries = append(f.queries, q)
	if !f.failed && strings.HasPrefix(q, f.failQuery) {
		f.failed = true
		return errBoom
	}
	return nil
}

var errBoom = stringError("boom")

type stringError string

func (s stringError) Error() string { return string(s) }
