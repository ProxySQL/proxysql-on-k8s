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
	"fmt"
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
		"DELETE FROM mysql_hostgroup_attributes",
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
		// Unset optional columns must get ProxySQL's NOT NULL defaults, not NULL
		// (weight=1, max_connections=1000, max_replication_lag=0, use_ssl=0) —
		// emitting NULL into these NOT NULL columns fails the constraint.
		"(1,'host-b',3306,1,1000,0,0,'')",
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

func TestSync_Quote_StripsNulAndControlBytes(t *testing.T) {
	rec := &recorder{}
	d := &Desired{
		// Password with an embedded NUL + a vertical-tab control char; comment
		// with a legitimate newline that must be preserved.
		MySQLUsers: []MySQLUser{
			{Username: "u\x00x", Password: "pa\x00ss\x0bword", Comment: "line1\nline2"},
		},
	}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	var insert string
	for _, q := range rec.queries {
		if strings.HasPrefix(q, "INSERT INTO mysql_users") {
			insert = q
			break
		}
	}
	if insert == "" {
		t.Fatalf("no INSERT into mysql_users; queries=%v", rec.queries)
	}
	if strings.ContainsRune(insert, 0) || strings.ContainsRune(insert, '\x0b') {
		t.Errorf("NUL or control byte survived into SQL: %q", insert)
	}
	// NUL stripped, not replaced: "u\x00x" -> "ux", "pa\x00ss\x0bword" -> "password".
	if !strings.Contains(insert, "'ux'") {
		t.Errorf("username NUL not stripped cleanly; got %q", insert)
	}
	if !strings.Contains(insert, "'password'") {
		t.Errorf("password control bytes not stripped cleanly; got %q", insert)
	}
	// A legitimate newline inside a comment must be preserved.
	if !strings.Contains(insert, "'line1\nline2'") {
		t.Errorf("newline in comment should be preserved; got %q", insert)
	}
}

// findInsert returns the first recorded query that is an INSERT into table.
func findInsert(t *testing.T, rec *recorder, table string) string {
	t.Helper()
	for _, q := range rec.queries {
		if strings.HasPrefix(q, "INSERT INTO "+table+" ") {
			return q
		}
	}
	t.Fatalf("no INSERT into %s issued; queries=%v", table, rec.queries)
	return ""
}

func TestSync_MySQLQueryRules_FullRule_RendersAllColumns(t *testing.T) {
	rec := &recorder{}
	active, apply, logQ, cacheEmpty := true, true, true, false
	flagIn, flagOut := int32(5), int32(6)
	dest, mirror := int32(2), int32(3)
	cacheTTL, timeout, delay := int32(5000), int32(1000), int32(10)
	d := &Desired{MySQLQueryRules: []MySQLQueryRule{{
		RuleID: 10, Active: &active, Username: "app", SchemaName: "appdb",
		FlagIn: &flagIn, MatchPattern: "^SELECT slow", MatchDigest: "dig",
		FlagOut: &flagOut, ReplacePattern: "SELECT fast",
		DestinationHostgroup: &dest, CacheTTL: &cacheTTL, CacheEmptyResult: &cacheEmpty,
		Timeout: &timeout, Delay: &delay, MirrorHostgroup: &mirror,
		ErrorMessage: "denied", Log: &logQ, Apply: &apply, Comment: "c",
	}}}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	insert := findInsert(t, rec, "mysql_query_rules")
	want := "INSERT INTO mysql_query_rules (rule_id,active,username,schemaname,flagIN," +
		"match_pattern,match_digest,flagOUT,replace_pattern,destination_hostgroup," +
		"cache_ttl,cache_empty_result,timeout,delay,mirror_hostgroup,error_msg,log,apply,comment) VALUES " +
		"(10,1,'app','appdb',5,'^SELECT slow','dig',6,'SELECT fast',2,5000,0,1000,10,3,'denied',1,1,'c')"
	if insert != want {
		t.Errorf("mysql_query_rules INSERT mismatch:\n got: %s\nwant: %s", insert, want)
	}
}

func TestSync_MySQLQueryRules_EmptyRule_DefaultsAndNulls(t *testing.T) {
	rec := &recorder{}
	d := &Desired{MySQLQueryRules: []MySQLQueryRule{{RuleID: 7}}}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	insert := findInsert(t, rec, "mysql_query_rules")
	// flagIN is NOT NULL DEFAULT 0 → 0; all genuinely nullable columns
	// (flagOUT, replace_pattern, destination_hostgroup, cache_ttl,
	// cache_empty_result, timeout, delay, mirror_hostgroup, error_msg, log)
	// must be NULL when unset. In particular replace_pattern='' would rewrite
	// matching queries to an empty string and a non-NULL error_msg would block
	// them — unset string fields must render as NULL, not ''.
	want := "(7,1,'','',0,'','',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,0,'')"
	if !strings.HasSuffix(insert, want) {
		t.Errorf("mysql_query_rules empty-rule row mismatch:\n got: %s\nwant suffix: %s", insert, want)
	}
}

func TestSync_PostgreSQLQueryRules_FullRule_RendersAllColumns(t *testing.T) {
	rec := &recorder{}
	active, apply, logQ, cacheEmpty := true, true, false, true
	flagIn, flagOut := int32(4), int32(9)
	dest, mirror := int32(1), int32(7)
	cacheTTL, timeout, delay := int32(250), int32(2000), int32(5)
	d := &Desired{PostgreSQLQueryRules: []PostgreSQLQueryRule{{
		RuleID: 20, Active: &active, FlagIn: &flagIn, MatchPattern: "^SELECT pg",
		FlagOut: &flagOut, ReplacePattern: "SELECT pg2",
		DestinationHostgroup: &dest, CacheTTL: &cacheTTL, CacheEmptyResult: &cacheEmpty,
		Timeout: &timeout, Delay: &delay, MirrorHostgroup: &mirror,
		ErrorMessage: "nope", Log: &logQ, Apply: &apply, Comment: "pgc",
	}}}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	insert := findInsert(t, rec, "pgsql_query_rules")
	want := "INSERT INTO pgsql_query_rules (rule_id,active,flagIN,match_pattern,flagOUT," +
		"replace_pattern,destination_hostgroup,cache_ttl,cache_empty_result,timeout,delay," +
		"mirror_hostgroup,error_msg,log,apply,comment) VALUES " +
		"(20,1,4,'^SELECT pg',9,'SELECT pg2',1,250,1,2000,5,7,'nope',0,1,'pgc')"
	if insert != want {
		t.Errorf("pgsql_query_rules INSERT mismatch:\n got: %s\nwant: %s", insert, want)
	}
}

func TestSync_PostgreSQLQueryRules_EmptyRule_DefaultsAndNulls(t *testing.T) {
	rec := &recorder{}
	d := &Desired{PostgreSQLQueryRules: []PostgreSQLQueryRule{{RuleID: 8}}}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	insert := findInsert(t, rec, "pgsql_query_rules")
	want := "(8,1,0,'',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,0,'')"
	if !strings.HasSuffix(insert, want) {
		t.Errorf("pgsql_query_rules empty-rule row mismatch:\n got: %s\nwant suffix: %s", insert, want)
	}
}

func TestSync_MySQLHostgroupAttributes_FullRow_RendersAllColumns(t *testing.T) {
	rec := &recorder{}
	maxOnline, autocommit, freePct, throttle := int32(5), int32(1), int32(20), int32(500)
	multiplex, warming := false, true
	d := &Desired{MySQLHostgroupAttributes: []MySQLHostgroupAttributes{{
		Hostgroup:           10,
		MaxNumOnlineServers: &maxOnline, Autocommit: &autocommit,
		FreeConnectionsPct: &freePct, InitConnect: "SET sql_mode=STRICT_ALL_TABLES",
		Multiplex: &multiplex, ConnectionWarming: &warming,
		ThrottleConnectionsPerSec: &throttle,
		IgnoreSessionVariables:    `["sql_log_bin"]`,
		Comment:                   "hg10",
	}}}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	insert := findInsert(t, rec, "mysql_hostgroup_attributes")
	want := "INSERT INTO mysql_hostgroup_attributes (hostgroup_id,max_num_online_servers," +
		"autocommit,free_connections_pct,init_connect,multiplex,connection_warming," +
		"throttle_connections_per_sec,ignore_session_variables,comment) VALUES " +
		"(10,5,1,20,'SET sql_mode=STRICT_ALL_TABLES',0,1,500,'[\"sql_log_bin\"]','hg10')"
	if insert != want {
		t.Errorf("mysql_hostgroup_attributes INSERT mismatch:\n got: %s\nwant: %s", insert, want)
	}
}

func TestSync_MySQLHostgroupAttributes_EmptyRow_RendersColumnDefaults(t *testing.T) {
	rec := &recorder{}
	d := &Desired{MySQLHostgroupAttributes: []MySQLHostgroupAttributes{{Hostgroup: 7}}}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	insert := findInsert(t, rec, "mysql_hostgroup_attributes")
	// Every column is NOT NULL with a ProxySQL default; unset fields must emit
	// those defaults (max_num_online_servers=1000000, autocommit=-1,
	// free_connections_pct=10, multiplex=1, connection_warming=0,
	// throttle_connections_per_sec=1000000, strings=''), never NULL.
	want := "(7,1000000,-1,10,'',1,0,1000000,'','')"
	if !strings.HasSuffix(insert, want) {
		t.Errorf("mysql_hostgroup_attributes empty-row mismatch:\n got: %s\nwant suffix: %s", insert, want)
	}
}

func TestSync_MySQLHostgroupAttributes_WrittenBeforeMySQLServersLoad(t *testing.T) {
	// mysql_hostgroup_attributes is part of the MYSQL SERVERS load/save
	// family (verified live: rows appear in runtime_mysql_hostgroup_attributes
	// only after LOAD MYSQL SERVERS TO RUNTIME). The table write must
	// therefore land before the shared mysql_servers_apply step.
	rec := &recorder{}
	d := &Desired{MySQLHostgroupAttributes: []MySQLHostgroupAttributes{{Hostgroup: 1}}}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	insertIdx, loadIdx := -1, -1
	for i, q := range rec.queries {
		if strings.HasPrefix(q, "INSERT INTO mysql_hostgroup_attributes ") {
			insertIdx = i
		}
		if q == "LOAD MYSQL SERVERS TO RUNTIME" {
			loadIdx = i
		}
	}
	if insertIdx < 0 || loadIdx < 0 {
		t.Fatalf("missing insert (%d) or load (%d); queries=%v", insertIdx, loadIdx, rec.queries)
	}
	if insertIdx > loadIdx {
		t.Errorf("mysql_hostgroup_attributes INSERT (idx %d) must precede LOAD MYSQL SERVERS TO RUNTIME (idx %d)", insertIdx, loadIdx)
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

// failOn wraps recorder and fails the first query containing failSubstr.
type failOn struct {
	recorder
	failSubstr string
}

func (f *failOn) Exec(ctx context.Context, q string, args ...any) error {
	if strings.Contains(q, f.failSubstr) {
		return fmt.Errorf("injected failure on %q", q)
	}
	return f.recorder.Exec(ctx, q, args...)
}

func indexOf(queries []string, substr string) int {
	for i, q := range queries {
		if strings.Contains(q, substr) {
			return i
		}
	}
	return -1
}

func TestSync_SQLStatements_VerbatimInOrderAfterVariables(t *testing.T) {
	rec := &recorder{}
	d := &Desired{
		AdminVariables: map[string]string{"admin-refresh_interval": "2000"},
		SQLStatements: []string{
			"UPDATE global_variables SET variable_value='250' WHERE variable_name='mysql-max_connections'",
			"LOAD MYSQL VARIABLES TO RUNTIME",
			"PROXYSQL FLUSH QUERY CACHE",
		},
	}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	iFlush := indexOf(rec.queries, "PROXYSQL FLUSH QUERY CACHE")
	iUpd := indexOf(rec.queries, "variable_name='mysql-max_connections'")
	iAdminApply := indexOf(rec.queries, "LOAD ADMIN VARIABLES TO RUNTIME")
	if iUpd == -1 || iFlush == -1 {
		t.Fatalf("statements not executed verbatim: %v", rec.queries)
	}
	if iUpd > iFlush {
		t.Fatalf("statements out of order: update at %d, flush at %d", iUpd, iFlush)
	}
	if iAdminApply == -1 || iUpd < iAdminApply {
		t.Fatalf("sqlStatements must run after structured variables (admin apply at %d, first statement at %d)", iAdminApply, iUpd)
	}
}

func TestSync_SQLStatements_FirstFailureAbortsRemainder(t *testing.T) {
	f := &failOn{failSubstr: "STATEMENT-B"}
	d := &Desired{SQLStatements: []string{
		"STATEMENT-A", "STATEMENT-B", "STATEMENT-C",
	}}
	err := Sync(context.Background(), f, d)
	if err == nil {
		t.Fatal("expected error from failing statement")
	}
	if !strings.Contains(err.Error(), "sqlStatements[1]") {
		t.Fatalf("error should name the failing statement index: %v", err)
	}
	if !f.seen("STATEMENT-A") {
		t.Fatal("statement before the failure must have executed")
	}
	if f.seen("STATEMENT-C") {
		t.Fatal("statement after the failure must NOT have executed")
	}
}

func TestSync_SQLStatements_EmptyIsNoOp(t *testing.T) {
	rec := &recorder{}
	if err := Sync(context.Background(), rec, &Desired{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if indexOf(rec.queries, "sqlStatements") != -1 {
		t.Fatalf("empty SQLStatements must add no queries")
	}
}
