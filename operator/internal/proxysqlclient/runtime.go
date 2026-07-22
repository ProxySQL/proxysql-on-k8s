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
	"strings"
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
//
// Servers are compared as MEMBERSHIP, not placement: for every hostgroup
// covered by a mysql_replication_hostgroups pair, a desired server counts as
// present when runtime holds it in ANY hostgroup of that pair's equivalence
// class. ProxySQL's read_only monitor legitimately moves servers between the
// writer and reader hostgroups of a pair (demotion, failover promotion,
// writer_is_also_reader mirroring) — re-pushing the spec's static placement
// against those moves would demote a just-promoted writer every resync
// (issue #34). A server absent from every hostgroup of its class, or an
// unknown server present in one, is real drift. Hostgroups outside every
// pair keep exact placement: with no pair there is no legitimate mover.
// pgsql_servers always use exact placement — this operator carries no
// PostgreSQL replication-hostgroup concept.
//
// mysql_replication_hostgroups, mysql_hostgroup_attributes and
// proxysql_servers are deliberately outside drift detection: the first two are
// loaded/saved together with mysql_servers (so external wipes of servers — the
// realistic mutation — are already caught), and the latter is peer topology
// that ProxySQL Cluster sync self-heals. All are still re-asserted whenever
// any drift triggers a push, since Sync always writes every table.
func (d *Desired) Drift(rs *RuntimeState) []string {
	classes := replicationClasses(d.MySQLReplicationHostgroups)
	diffs := make([]string, 0, 8)
	diffs = append(diffs, diffServers("mysql_servers", mysqlServerKeys(d.MySQLServers, classes), runtimeServerKeys(rs.MySQLServers, classes))...)
	diffs = append(diffs, diffKeys("mysql_users", userKeys(mysqlUsernames(d.MySQLUsers)), rs.MySQLUsers)...)
	diffs = append(diffs, diffKeys("mysql_query_rules", ruleKeys(mysqlRuleIDs(d.MySQLQueryRules)), rs.MySQLRules)...)
	diffs = append(diffs, diffServers("pgsql_servers", pgsqlServerKeys(d.PostgreSQLServers), runtimeServerKeys(rs.PgSQLServers, nil))...)
	diffs = append(diffs, diffKeys("pgsql_users", userKeys(pgsqlUsernames(d.PostgreSQLUsers)), rs.PgSQLUsers)...)
	diffs = append(diffs, diffKeys("pgsql_query_rules", ruleKeys(pgsqlRuleIDs(d.PostgreSQLQueryRules)), rs.PgSQLRules)...)
	sort.Strings(diffs)
	return diffs
}

// ---- replication-hostgroup equivalence ----

// replicationClasses maps every hostgroup covered by a
// mysql_replication_hostgroups pair to a canonical class representative (the
// smallest hostgroup id in its class). Pairs sharing a hostgroup chain into
// one class via union-find — ProxySQL treats hostgroups reachable through
// shared pairs as one replication topology. Returns nil when no pairs are
// configured, which keeps server comparison exact.
func replicationClasses(pairs []MySQLReplicationHostgroup) map[int32]int32 {
	if len(pairs) == 0 {
		return nil
	}
	parent := map[int32]int32{}
	var find func(x int32) int32
	find = func(x int32) int32 {
		p, ok := parent[x]
		if !ok {
			parent[x] = x
			return x
		}
		if p == x {
			return x
		}
		root := find(p)
		parent[x] = root
		return root
	}
	for _, p := range pairs {
		rw, rr := find(p.WriterHostgroup), find(p.ReaderHostgroup)
		if rw != rr {
			if rr < rw {
				rw, rr = rr, rw
			}
			parent[rr] = rw // smaller id becomes the representative
		}
	}
	classes := make(map[int32]int32, len(parent))
	for hg := range parent {
		classes[hg] = find(hg)
	}
	return classes
}

// canonServerKey returns the comparison key for a server row: hostgroups in a
// replication class compare by the class representative ("rhg<rep>:host:port",
// a prefix no literal hostgroup id can produce), everything else by the exact
// "hg:host:port" identity.
func canonServerKey(hg int32, hostPort string, classes map[int32]int32) string {
	if rep, ok := classes[hg]; ok {
		return fmt.Sprintf("rhg%d:%s", rep, hostPort)
	}
	return fmt.Sprintf("%d:%s", hg, hostPort)
}

// ---- key builders ----
//
// Server key maps are canonical-key → display-key: the canonical key drives
// the membership comparison, the display key keeps drift messages naming the
// concrete row (spec placement for "missing", runtime row for "extra"). When
// several rows collapse onto one canonical key (writer_is_also_reader), the
// lexicographically smallest display wins for determinism.

func mysqlServerKeys(servers []MySQLServer, classes map[int32]int32) map[string]string {
	keys := make(map[string]string, len(servers))
	for _, s := range servers {
		hostPort := fmt.Sprintf("%s:%d", s.Hostname, defaultInt32(s.Port, 3306))
		display := fmt.Sprintf("%d:%s", s.Hostgroup, hostPort)
		canon := canonServerKey(s.Hostgroup, hostPort, classes)
		if prev, ok := keys[canon]; !ok || display < prev {
			keys[canon] = display
		}
	}
	return keys
}

func pgsqlServerKeys(servers []PostgreSQLServer) map[string]string {
	keys := make(map[string]string, len(servers))
	for _, s := range servers {
		k := fmt.Sprintf("%d:%s:%d", s.Hostgroup, s.Hostname, defaultInt32(s.Port, 5432))
		keys[k] = k
	}
	return keys
}

// runtimeServerKeys canonicalizes a runtime "hg:host:port"→status map for
// comparison. Only the leading hostgroup id is parsed — the host:port tail is
// carried verbatim, so IPv6 hostnames survive.
func runtimeServerKeys(have map[string]string, classes map[int32]int32) map[string]string {
	keys := make(map[string]string, len(have))
	for k := range have {
		canon := k
		if len(classes) > 0 {
			if i := strings.IndexByte(k, ':'); i > 0 {
				if hg, err := strconv.ParseInt(k[:i], 10, 32); err == nil {
					canon = canonServerKey(int32(hg), k[i+1:], classes)
				}
			}
		}
		if prev, ok := keys[canon]; !ok || k < prev {
			keys[canon] = k
		}
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

// diffServers is diffKeys over canonical-key → display-key maps: membership
// is decided on the canonical keys, messages carry the display keys. Runtime
// status never participates — presence is what matters.
func diffServers(table string, want, have map[string]string) []string {
	var diffs []string
	for canon, display := range want {
		if _, ok := have[canon]; !ok {
			diffs = append(diffs, table+": missing "+display)
		}
	}
	for canon, display := range have {
		if _, ok := want[canon]; !ok {
			diffs = append(diffs, table+": extra "+display)
		}
	}
	return diffs
}

// ReadGlobalVariables returns variable_name→variable_value from
// runtime_global_variables for exactly the requested names.
func ReadGlobalVariables(ctx context.Context, q Querier, names []string) (map[string]string, error) {
	result := make(map[string]string)
	if len(names) == 0 {
		return result, nil
	}

	// Sort names for deterministic query
	sortedNames := make([]string, len(names))
	copy(sortedNames, names)
	sort.Strings(sortedNames)

	// Build IN clause with quoted, sorted names
	var inClause []string
	for _, name := range sortedNames {
		inClause = append(inClause, quote(name))
	}

	query := fmt.Sprintf(
		"SELECT variable_name, variable_value FROM runtime_global_variables WHERE variable_name IN (%s)",
		strings.Join(inClause, ","),
	)

	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		if len(row) >= 2 {
			result[row[0]] = row[1]
		}
	}

	return result, nil
}
