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

// Executor is the subset of *Client that Sync needs. Defined as an interface
// so tests can substitute a recording fake without dialing a real ProxySQL.
type Executor interface {
	Exec(ctx context.Context, query string, args ...any) error
}

// Sync applies the full desired state to a single ProxySQL admin endpoint.
// The pattern for every section is:
//
//  1. DELETE FROM <table>
//  2. INSERT INTO <table> ... (rows)
//  3. LOAD <X> TO RUNTIME ; SAVE <X> TO DISK
//
// Variables are applied via UPDATE on global_variables.
//
// Each section is independent: if mysql_users fails, mysql_servers stays
// applied. Failures are aggregated; the first error is returned.
//
// sql_statements runs last, after every structured section above; within
// it, the first failing statement aborts the remaining statements in the list.
func Sync(ctx context.Context, c Executor, d *Desired) error {
	steps := []syncStep{
		{name: "mysql_servers", run: func() error { return syncMySQLServers(ctx, c, d) }},
		{name: "mysql_replication_hostgroups", run: func() error { return syncMySQLReplicationHostgroups(ctx, c, d) }},
		{name: "mysql_hostgroup_attributes", run: func() error { return syncMySQLHostgroupAttributes(ctx, c, d) }},
		// mysql_replication_hostgroups and mysql_hostgroup_attributes are
		// loaded with mysql_servers (verified live: runtime rows appear only
		// after LOAD MYSQL SERVERS TO RUNTIME); apply the LOAD/SAVE only once
		// after all three tables are written.
		{name: "mysql_servers_apply", run: func() error { return loadSave(ctx, c, "MYSQL SERVERS") }},

		{name: "mysql_users", run: func() error { return syncMySQLUsers(ctx, c, d) }},
		{name: "mysql_users_apply", run: func() error { return loadSave(ctx, c, "MYSQL USERS") }},

		{name: "mysql_query_rules", run: func() error { return syncMySQLQueryRules(ctx, c, d) }},
		{name: "mysql_query_rules_apply", run: func() error { return loadSave(ctx, c, "MYSQL QUERY RULES") }},

		{name: "pgsql_servers", run: func() error { return syncPostgreSQLServers(ctx, c, d) }},
		{name: "pgsql_servers_apply", run: func() error { return loadSave(ctx, c, "PGSQL SERVERS") }},

		{name: "pgsql_users", run: func() error { return syncPostgreSQLUsers(ctx, c, d) }},
		{name: "pgsql_users_apply", run: func() error { return loadSave(ctx, c, "PGSQL USERS") }},

		{name: "pgsql_query_rules", run: func() error { return syncPostgreSQLQueryRules(ctx, c, d) }},
		{name: "pgsql_query_rules_apply", run: func() error { return loadSave(ctx, c, "PGSQL QUERY RULES") }},

		{name: "proxysql_servers", run: func() error { return syncProxySQLServers(ctx, c, d) }},
		{name: "proxysql_servers_apply", run: func() error { return loadSave(ctx, c, "PROXYSQL SERVERS") }},

		{name: "mysql_variables", run: func() error { return syncVariables(ctx, c, d.MySQLVariables, "MYSQL") }},
		{name: "pgsql_variables", run: func() error { return syncVariables(ctx, c, d.PostgreSQLVariables, "PGSQL") }},
		{name: "admin_variables", run: func() error { return syncVariables(ctx, c, d.AdminVariables, "ADMIN") }},
		{name: "sql_statements", run: func() error { return syncSQLStatements(ctx, c, d) }},
	}

	var firstErr error
	for _, s := range steps {
		if err := s.run(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", s.name, err)
			}
		}
	}
	return firstErr
}

type syncStep struct {
	name string
	run  func() error
}

func loadSave(ctx context.Context, c Executor, what string) error {
	if err := c.Exec(ctx, "LOAD "+what+" TO RUNTIME"); err != nil {
		return err
	}
	return c.Exec(ctx, "SAVE "+what+" TO DISK")
}

// ---- mysql_servers ----

func syncMySQLServers(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM mysql_servers"); err != nil {
		return err
	}
	if len(d.MySQLServers) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO mysql_servers (hostgroup_id,hostname,port,weight,max_connections,max_replication_lag,use_ssl,comment) VALUES ")
	for i, s := range d.MySQLServers {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%s,%d,%s,%s,%s,%s,%s)",
			s.Hostgroup,
			quote(s.Hostname),
			defaultInt32(s.Port, 3306),
			defInt32(s.Weight, 1),            // mysql_servers.weight NOT NULL DEFAULT 1
			defInt32(s.MaxConnections, 1000), // NOT NULL DEFAULT 1000
			defInt32(s.MaxReplicationLag, 0), // NOT NULL DEFAULT 0
			defBoolAsInt(s.UseSSL, false),    // NOT NULL DEFAULT 0
			quote(s.Comment),
		)
	}
	return c.Exec(ctx, b.String())
}

// ---- mysql_replication_hostgroups ----

func syncMySQLReplicationHostgroups(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM mysql_replication_hostgroups"); err != nil {
		return err
	}
	if len(d.MySQLReplicationHostgroups) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO mysql_replication_hostgroups (writer_hostgroup,reader_hostgroup,check_type,comment) VALUES ")
	for i, h := range d.MySQLReplicationHostgroups {
		if i > 0 {
			b.WriteByte(',')
		}
		check := h.CheckType
		if check == "" {
			check = "read_only"
		}
		fmt.Fprintf(&b, "(%d,%d,%s,%s)", h.WriterHostgroup, h.ReaderHostgroup, quote(check), quote(h.Comment))
	}
	return c.Exec(ctx, b.String())
}

// ---- mysql_hostgroup_attributes ----

func syncMySQLHostgroupAttributes(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM mysql_hostgroup_attributes"); err != nil {
		return err
	}
	if len(d.MySQLHostgroupAttributes) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO mysql_hostgroup_attributes (hostgroup_id,max_num_online_servers," +
		"autocommit,free_connections_pct,init_connect,multiplex,connection_warming," +
		"throttle_connections_per_sec,ignore_session_variables,comment) VALUES ")
	for i, a := range d.MySQLHostgroupAttributes {
		if i > 0 {
			b.WriteByte(',')
		}
		// Every column is NOT NULL with a ProxySQL default — render the
		// column default for unset fields, never NULL.
		fmt.Fprintf(&b, "(%d,%s,%s,%s,%s,%s,%s,%s,%s,%s)",
			a.Hostgroup,
			defInt32(a.MaxNumOnlineServers, 1000000),       // NOT NULL DEFAULT 1000000
			defInt32(a.Autocommit, -1),                     // -1|0|1, NOT NULL DEFAULT -1 (-1 = don't enforce)
			defInt32(a.FreeConnectionsPct, 10),             // 0..100, NOT NULL DEFAULT 10
			quote(a.InitConnect),                           // NOT NULL DEFAULT ''
			defBoolAsInt(a.Multiplex, true),                // NOT NULL DEFAULT 1
			defBoolAsInt(a.ConnectionWarming, false),       // NOT NULL DEFAULT 0
			defInt32(a.ThrottleConnectionsPerSec, 1000000), // 1..1000000, NOT NULL DEFAULT 1000000
			quote(a.IgnoreSessionVariables),                // JSON or '', NOT NULL DEFAULT ''
			quote(a.Comment),
		)
	}
	return c.Exec(ctx, b.String())
}

// ---- mysql_users ----

func syncMySQLUsers(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM mysql_users"); err != nil {
		return err
	}
	if len(d.MySQLUsers) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO mysql_users (username,password,default_hostgroup,active,max_connections,use_ssl,default_schema,transaction_persistent,comment) VALUES ")
	for i, u := range d.MySQLUsers {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%s,%s,%d,%s,%s,%s,%s,%s,%s)",
			quote(u.Username),
			quote(u.Password),
			u.DefaultHostgroup,
			defBoolAsInt(u.Active, true),                // mysql_users.active NOT NULL DEFAULT 1
			defInt32(u.MaxConnections, 10000),           // NOT NULL DEFAULT 10000
			defBoolAsInt(u.UseSSL, false),               // NOT NULL DEFAULT 0
			quote(u.DefaultSchema),                      // nullable column; '' is fine
			defBoolAsInt(u.TransactionPersistent, true), // NOT NULL DEFAULT 1
			quote(u.Comment),
		)
	}
	return c.Exec(ctx, b.String())
}

// ---- mysql_query_rules ----

func syncMySQLQueryRules(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM mysql_query_rules"); err != nil {
		return err
	}
	if len(d.MySQLQueryRules) == 0 {
		return nil
	}
	// Sort by RuleID for deterministic ordering, even though ProxySQL
	// stores its own order — keeps diffs stable and easier to reason about.
	rules := append([]MySQLQueryRule(nil), d.MySQLQueryRules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })

	var b strings.Builder
	b.WriteString("INSERT INTO mysql_query_rules (rule_id,active,username,schemaname,flagIN," +
		"match_pattern,match_digest,flagOUT,replace_pattern,destination_hostgroup," +
		"cache_ttl,cache_empty_result,timeout,delay,mirror_hostgroup,error_msg,log,apply,comment) VALUES ")
	for i, r := range rules {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)",
			r.RuleID,
			defBoolAsInt(r.Active, true), // a declared rule defaults to active
			quote(r.Username),
			quote(r.SchemaName),
			defInt32(r.FlagIn, 0), // flagIN NOT NULL DEFAULT 0 (chain entry point)
			quote(r.MatchPattern),
			quote(r.MatchDigest),
			nullableInt32(r.FlagOut),              // nullable: NULL = keep current flag
			nullableString(r.ReplacePattern),      // nullable: '' would rewrite to empty query
			nullableInt32(r.DestinationHostgroup), // nullable: NULL = no hostgroup override
			nullableInt32(r.CacheTTL),             // nullable: NULL = no caching
			nullableBoolAsInt(r.CacheEmptyResult), // nullable
			nullableInt32(r.Timeout),              // nullable: NULL = global default
			nullableInt32(r.Delay),                // nullable: NULL = no delay
			nullableInt32(r.MirrorHostgroup),      // nullable: NULL = no mirroring
			nullableString(r.ErrorMessage),        // nullable: non-NULL blocks the query
			nullableBoolAsInt(r.Log),              // nullable
			defBoolAsInt(r.Apply, false),          // mysql_query_rules.apply NOT NULL DEFAULT 0
			quote(r.Comment),
		)
	}
	return c.Exec(ctx, b.String())
}

// ---- pgsql_servers ----

func syncPostgreSQLServers(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM pgsql_servers"); err != nil {
		return err
	}
	if len(d.PostgreSQLServers) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO pgsql_servers (hostgroup_id,hostname,port,weight,max_connections,use_ssl,comment) VALUES ")
	for i, s := range d.PostgreSQLServers {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%s,%d,%s,%s,%s,%s)",
			s.Hostgroup,
			quote(s.Hostname),
			defaultInt32(s.Port, 5432),
			defInt32(s.Weight, 1),            // pgsql_servers.weight NOT NULL DEFAULT 1
			defInt32(s.MaxConnections, 1000), // NOT NULL DEFAULT 1000
			defBoolAsInt(s.UseSSL, false),    // NOT NULL DEFAULT 0
			quote(s.Comment),
		)
	}
	return c.Exec(ctx, b.String())
}

// ---- pgsql_users ----

func syncPostgreSQLUsers(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM pgsql_users"); err != nil {
		return err
	}
	if len(d.PostgreSQLUsers) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO pgsql_users (username,password,default_hostgroup,active,comment) VALUES ")
	for i, u := range d.PostgreSQLUsers {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%s,%s,%d,%s,%s)",
			quote(u.Username),
			quote(u.Password),
			u.DefaultHostgroup,
			defBoolAsInt(u.Active, true), // pgsql_users.active NOT NULL DEFAULT 1
			quote(u.Comment),
		)
	}
	return c.Exec(ctx, b.String())
}

// ---- pgsql_query_rules ----

func syncPostgreSQLQueryRules(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM pgsql_query_rules"); err != nil {
		return err
	}
	if len(d.PostgreSQLQueryRules) == 0 {
		return nil
	}
	rules := append([]PostgreSQLQueryRule(nil), d.PostgreSQLQueryRules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })

	var b strings.Builder
	b.WriteString("INSERT INTO pgsql_query_rules (rule_id,active,flagIN,match_pattern,flagOUT," +
		"replace_pattern,destination_hostgroup,cache_ttl,cache_empty_result,timeout,delay," +
		"mirror_hostgroup,error_msg,log,apply,comment) VALUES ")
	for i, r := range rules {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)",
			r.RuleID,
			defBoolAsInt(r.Active, true), // a declared rule defaults to active
			defInt32(r.FlagIn, 0),        // flagIN NOT NULL DEFAULT 0 (chain entry point)
			quote(r.MatchPattern),
			nullableInt32(r.FlagOut),              // nullable: NULL = keep current flag
			nullableString(r.ReplacePattern),      // nullable: '' would rewrite to empty query
			nullableInt32(r.DestinationHostgroup), // nullable
			nullableInt32(r.CacheTTL),             // nullable: NULL = no caching
			nullableBoolAsInt(r.CacheEmptyResult), // nullable
			nullableInt32(r.Timeout),              // nullable: NULL = global default
			nullableInt32(r.Delay),                // nullable: NULL = no delay
			nullableInt32(r.MirrorHostgroup),      // nullable: NULL = no mirroring
			nullableString(r.ErrorMessage),        // nullable: non-NULL blocks the query
			nullableBoolAsInt(r.Log),              // nullable
			defBoolAsInt(r.Apply, false),          // pgsql_query_rules.apply NOT NULL DEFAULT 0
			quote(r.Comment),
		)
	}
	return c.Exec(ctx, b.String())
}

// ---- proxysql_servers ----

func syncProxySQLServers(ctx context.Context, c Executor, d *Desired) error {
	if err := c.Exec(ctx, "DELETE FROM proxysql_servers"); err != nil {
		return err
	}
	if len(d.ProxySQLServers) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO proxysql_servers (hostname,port,weight,comment) VALUES ")
	for i, s := range d.ProxySQLServers {
		if i > 0 {
			b.WriteByte(',')
		}
		port := s.Port
		if port == 0 {
			port = 6032
		}
		fmt.Fprintf(&b, "(%s,%d,%d,%s)", quote(s.Hostname), port, s.Weight, quote(s.Comment))
	}
	return c.Exec(ctx, b.String())
}

// ---- variables ----

// syncVariables applies a map of variable_name=variable_value to
// global_variables and issues LOAD/SAVE on the named domain (MYSQL/PGSQL/ADMIN).
// No-op when the map is empty (variables retain whatever ProxySQL was last told).
func syncVariables(ctx context.Context, c Executor, vars map[string]string, domain string) error {
	if len(vars) == 0 {
		return nil
	}
	// Apply in sorted key order so logs and any retries are deterministic.
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := vars[k]
		q := fmt.Sprintf("UPDATE global_variables SET variable_value=%s WHERE variable_name=%s",
			quote(v), quote(k))
		if err := c.Exec(ctx, q); err != nil {
			return err
		}
	}
	return loadSave(ctx, c, domain+" VARIABLES")
}

// ApplyVariables pushes full-named variables ("mysql-max_connections") for
// one domain ("MYSQL"|"PGSQL"|"ADMIN") and loads+saves them. Thin exported
// wrapper over the sync path's variable step.
func ApplyVariables(ctx context.Context, c Executor, vars map[string]string, domain string) error {
	return syncVariables(ctx, c, vars, domain)
}

// ReloadTLS issues PROXYSQL RELOAD TLS: ProxySQL re-reads the fixed datadir
// certificate files (proxysql-{ca,cert,key}.pem — for operator-managed
// clusters, symlinks into the mounted TLS Secret) and starts serving the
// new material on every listener with zero restarts. Self-contained: no
// LOAD/SAVE cycle applies to it.
func ReloadTLS(ctx context.Context, c Executor) error {
	return c.Exec(ctx, "PROXYSQL RELOAD TLS")
}

// syncSQLStatements executes user-provided raw admin SQL in listed order.
// Unlike the table sections, the first failure aborts the remaining
// statements: order may carry dependencies (e.g. UPDATE then LOAD).
func syncSQLStatements(ctx context.Context, c Executor, d *Desired) error {
	for i, stmt := range d.SQLStatements {
		if err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("sqlStatements[%d]: %w", i, err)
		}
	}
	return nil
}

// ---- helpers ----

// quote returns the SQL-literal form of s: 'value' with internal quotes
// doubled. Used everywhere instead of parameter binding because ProxySQL's
// admin parser handles literal SQL better than prepared statements.
//
// Single-quote doubling is sufficient against breakout for ProxySQL's
// SQLite-derived admin parser. As defense-in-depth we also strip NUL and other
// C0 control characters first: a NUL embedded in a password or comment can be
// truncated by ProxySQL's C/C++ string handling (silently storing a different,
// shorter value than intended), and stray control bytes have no legitimate
// place in a hostname/username/password/comment. Tab, newline, and carriage
// return are preserved since they can legitimately appear in a comment.
func quote(s string) string {
	return "'" + strings.ReplaceAll(stripControl(s), "'", "''") + "'"
}

// stripControl removes NUL and C0 control characters (except \t, \n, \r) from s.
func stripControl(s string) string {
	if strings.IndexFunc(s, isStripControl) < 0 {
		return s // common case: nothing to strip, no allocation
	}
	return strings.Map(func(r rune) rune {
		if isStripControl(r) {
			return -1
		}
		return r
	}, s)
}

func isStripControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f
}

// sqlNull is the literal emitted for unset values in genuinely nullable columns.
const sqlNull = "NULL"

func defaultInt32(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

// nullableInt32 emits *p, or NULL when unset. Use ONLY for genuinely nullable
// ProxySQL columns (e.g. mysql_query_rules.destination_hostgroup). For NOT NULL
// columns use defInt32/defBoolAsInt — emitting NULL into a NOT NULL column
// fails with "NOT NULL constraint failed".
func nullableInt32(p *int32) string {
	if p == nil {
		return sqlNull
	}
	return strconv.FormatInt(int64(*p), 10)
}

// defInt32 emits *p, or def when unset. For NOT NULL integer columns, def must
// be ProxySQL's column default so an unset field behaves as the backend default.
func defInt32(p *int32, def int32) string {
	if p == nil {
		return strconv.FormatInt(int64(def), 10)
	}
	return strconv.FormatInt(int64(*p), 10)
}

// nullableBoolAsInt emits *p as 0/1, or NULL when unset. Use ONLY for genuinely
// nullable ProxySQL boolean columns (e.g. mysql_query_rules.log) where NULL
// means "unset / inherit default behavior" — semantically different from 0.
func nullableBoolAsInt(p *bool) string {
	if p == nil {
		return sqlNull
	}
	if *p {
		return "1"
	}
	return "0"
}

// nullableString emits the quoted form of s, or NULL when s is empty. Use ONLY
// for nullable string columns where ” and NULL behave differently in ProxySQL:
// replace_pattern=” rewrites matching queries to an empty string, and any
// non-NULL error_msg (even ”) blocks matching queries. An unset field must
// therefore render as NULL, not ”.
func nullableString(s string) string {
	if s == "" {
		return sqlNull
	}
	return quote(s)
}

// defBoolAsInt emits *p as 0/1, or def when unset. For NOT NULL boolean columns.
func defBoolAsInt(p *bool, def bool) string {
	v := def
	if p != nil {
		v = *p
	}
	if v {
		return "1"
	}
	return "0"
}
