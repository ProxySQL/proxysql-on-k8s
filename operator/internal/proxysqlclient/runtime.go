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
	"sort"
	"strconv"
)

// Querier is the subset of *Client that ReadRuntime needs. Defined as an
// interface so tests can substitute a canned-row fake without dialing a real
// ProxySQL.
type Querier interface {
	Query(ctx context.Context, query string) ([][]string, error)
}

// RuntimeState is a keys-only snapshot of what a single ProxySQL instance is
// actually running. Keys only: attribute-level changes (weights, comments,
// max_connections, ...) are carried by the spec-hash path in the controller,
// so read-back deliberately compares just the identities — server
// "hostgroup:hostname:port", username, rule id. Server maps additionally
// carry the runtime status so callers can count SHUNNED backends.
//
// Passwords are never read back; the queries below select identity columns
// only.
type RuntimeState struct {
	// MySQLServers maps "hostgroup:hostname:port" to the runtime status
	// (ONLINE, SHUNNED, OFFLINE_SOFT, ...).
	MySQLServers map[string]string
	MySQLUsers   map[string]bool
	MySQLRules   map[string]bool

	// PgSQLServers maps "hostgroup:hostname:port" to the runtime status.
	PgSQLServers map[string]string
	PgSQLUsers   map[string]bool
	PgSQLRules   map[string]bool
}

// ReadRuntime snapshots the runtime_* admin tables of a single ProxySQL
// instance. Any query error aborts the read — a partial snapshot would look
// like mass drift.
//
// runtime_mysql_users holds a frontend AND a backend row per user, hence the
// SELECT DISTINCT. Password columns are never selected.
func ReadRuntime(ctx context.Context, q Querier) (*RuntimeState, error) {
	rs := &RuntimeState{
		MySQLServers: map[string]string{},
		MySQLUsers:   map[string]bool{},
		MySQLRules:   map[string]bool{},
		PgSQLServers: map[string]string{},
		PgSQLUsers:   map[string]bool{},
		PgSQLRules:   map[string]bool{},
	}

	if err := readServers(ctx, q, "runtime_mysql_servers", rs.MySQLServers); err != nil {
		return nil, err
	}
	if err := readKeys(ctx, q, "SELECT DISTINCT username FROM runtime_mysql_users", rs.MySQLUsers); err != nil {
		return nil, err
	}
	if err := readKeys(ctx, q, "SELECT rule_id FROM runtime_mysql_query_rules", rs.MySQLRules); err != nil {
		return nil, err
	}

	if err := readServers(ctx, q, "runtime_pgsql_servers", rs.PgSQLServers); err != nil {
		return nil, err
	}
	if err := readKeys(ctx, q, "SELECT DISTINCT username FROM runtime_pgsql_users", rs.PgSQLUsers); err != nil {
		return nil, err
	}
	if err := readKeys(ctx, q, "SELECT rule_id FROM runtime_pgsql_query_rules", rs.PgSQLRules); err != nil {
		return nil, err
	}

	return rs, nil
}

// readServers fills dst with "hostgroup:hostname:port" → status from a
// runtime servers table.
func readServers(ctx context.Context, q Querier, table string, dst map[string]string) error {
	rows, err := q.Query(ctx, "SELECT hostgroup_id, hostname, port, status FROM "+table)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if len(r) >= 4 {
			dst[r[0]+":"+r[1]+":"+r[2]] = r[3]
		}
	}
	return nil
}

// readKeys fills dst with the first column of every row returned by query.
func readKeys(ctx context.Context, q Querier, query string, dst map[string]bool) error {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if len(r) >= 1 {
			dst[r[0]] = true
		}
	}
	return nil
}

// ShunnedCount returns the number of backend servers (MySQL + PostgreSQL)
// whose runtime status is SHUNNED.
func (rs *RuntimeState) ShunnedCount() int32 {
	var n int32
	for _, status := range rs.MySQLServers {
		if status == "SHUNNED" {
			n++
		}
	}
	for _, status := range rs.PgSQLServers {
		if status == "SHUNNED" {
			n++
		}
	}
	return n
}

// Drift compares the desired keys against a runtime snapshot and returns a
// sorted list of human-readable differences ("<table>: missing <key>" /
// "<table>: extra <key>"). Empty means converged.
//
// Comparison is keys-only by design: see RuntimeState. A SHUNNED server is
// present, not drifted — shunning is ProxySQL's own health reaction, not a
// config divergence.
func (d *Desired) Drift(rs *RuntimeState) []string {
	diffs := make([]string, 0, 8)
	diffs = append(diffs, diffServers("mysql_servers", mysqlServerKeys(d.MySQLServers), rs.MySQLServers)...)
	diffs = append(diffs, diffKeys("mysql_users", userKeys(mysqlUsernames(d.MySQLUsers)), rs.MySQLUsers)...)
	diffs = append(diffs, diffKeys("mysql_query_rules", ruleKeys(mysqlRuleIDs(d.MySQLQueryRules)), rs.MySQLRules)...)
	diffs = append(diffs, diffServers("pgsql_servers", pgsqlServerKeys(d.PostgreSQLServers), rs.PgSQLServers)...)
	diffs = append(diffs, diffKeys("pgsql_users", userKeys(pgsqlUsernames(d.PostgreSQLUsers)), rs.PgSQLUsers)...)
	diffs = append(diffs, diffKeys("pgsql_query_rules", ruleKeys(pgsqlRuleIDs(d.PostgreSQLQueryRules)), rs.PgSQLRules)...)
	sort.Strings(diffs)
	return diffs
}

// ---- key builders ----

func mysqlServerKeys(servers []MySQLServer) map[string]bool {
	keys := make(map[string]bool, len(servers))
	for _, s := range servers {
		keys[fmt.Sprintf("%d:%s:%d", s.Hostgroup, s.Hostname, defaultInt32(s.Port, 3306))] = true
	}
	return keys
}

func pgsqlServerKeys(servers []PostgreSQLServer) map[string]bool {
	keys := make(map[string]bool, len(servers))
	for _, s := range servers {
		keys[fmt.Sprintf("%d:%s:%d", s.Hostgroup, s.Hostname, defaultInt32(s.Port, 5432))] = true
	}
	return keys
}

func mysqlUsernames(users []MySQLUser) []string {
	names := make([]string, len(users))
	for i, u := range users {
		names[i] = u.Username
	}
	return names
}

func pgsqlUsernames(users []PostgreSQLUser) []string {
	names := make([]string, len(users))
	for i, u := range users {
		names[i] = u.Username
	}
	return names
}

func mysqlRuleIDs(rules []MySQLQueryRule) []int32 {
	ids := make([]int32, len(rules))
	for i, r := range rules {
		ids[i] = r.RuleID
	}
	return ids
}

func pgsqlRuleIDs(rules []PostgreSQLQueryRule) []int32 {
	ids := make([]int32, len(rules))
	for i, r := range rules {
		ids[i] = r.RuleID
	}
	return ids
}

func userKeys(names []string) map[string]bool {
	keys := make(map[string]bool, len(names))
	for _, n := range names {
		keys[n] = true
	}
	return keys
}

func ruleKeys(ids []int32) map[string]bool {
	keys := make(map[string]bool, len(ids))
	for _, id := range ids {
		keys[strconv.FormatInt(int64(id), 10)] = true
	}
	return keys
}

// ---- diff helpers ----

// diffKeys reports want-keys absent from have ("missing") and have-keys
// absent from want ("extra"). Order is left to the caller's sort.
func diffKeys(table string, want, have map[string]bool) []string {
	var diffs []string
	for k := range want {
		if !have[k] {
			diffs = append(diffs, table+": missing "+k)
		}
	}
	for k := range have {
		if !want[k] {
			diffs = append(diffs, table+": extra "+k)
		}
	}
	return diffs
}

// diffServers is diffKeys against a status-valued runtime map; the status is
// ignored — presence is what matters.
func diffServers(table string, want map[string]bool, have map[string]string) []string {
	haveKeys := make(map[string]bool, len(have))
	for k := range have {
		haveKeys[k] = true
	}
	return diffKeys(table, want, haveKeys)
}
