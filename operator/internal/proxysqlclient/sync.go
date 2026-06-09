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
func Sync(ctx context.Context, c Executor, d *Desired) error {
	steps := []syncStep{
		{name: "mysql_servers", run: func() error { return syncMySQLServers(ctx, c, d) }},
		{name: "mysql_replication_hostgroups", run: func() error { return syncMySQLReplicationHostgroups(ctx, c, d) }},
		// mysql_replication_hostgroups is loaded with mysql_servers; apply
		// the LOAD/SAVE only once after both tables are written.
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
			nullableInt32(s.Weight),
			nullableInt32(s.MaxConnections),
			nullableInt32(s.MaxReplicationLag),
			nullableBoolAsInt(s.UseSSL),
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
			nullableBoolAsInt(u.Active),
			nullableInt32(u.MaxConnections),
			nullableBoolAsInt(u.UseSSL),
			quote(u.DefaultSchema),
			nullableBoolAsInt(u.TransactionPersistent),
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
	b.WriteString("INSERT INTO mysql_query_rules (rule_id,active,username,schemaname,match_pattern,match_digest,destination_hostgroup,apply,comment) VALUES ")
	for i, r := range rules {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%s,%s,%s,%s,%s,%s,%s,%s)",
			r.RuleID,
			nullableBoolAsInt(r.Active),
			quote(r.Username),
			quote(r.SchemaName),
			quote(r.MatchPattern),
			quote(r.MatchDigest),
			nullableInt32(r.DestinationHostgroup),
			nullableBoolAsInt(r.Apply),
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
	b.WriteString("INSERT INTO pgsql_servers (hostgroup_id,hostname,port,weight,max_connections,comment) VALUES ")
	for i, s := range d.PostgreSQLServers {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%s,%d,%s,%s,%s)",
			s.Hostgroup,
			quote(s.Hostname),
			defaultInt32(s.Port, 5432),
			nullableInt32(s.Weight),
			nullableInt32(s.MaxConnections),
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
			nullableBoolAsInt(u.Active),
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
	b.WriteString("INSERT INTO pgsql_query_rules (rule_id,active,match_pattern,destination_hostgroup,apply,comment) VALUES ")
	for i, r := range rules {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%s,%s,%s,%s,%s)",
			r.RuleID,
			nullableBoolAsInt(r.Active),
			quote(r.MatchPattern),
			nullableInt32(r.DestinationHostgroup),
			nullableBoolAsInt(r.Apply),
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

func defaultInt32(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

func nullableInt32(p *int32) string {
	if p == nil {
		return "NULL"
	}
	return strconv.FormatInt(int64(*p), 10)
}

func nullableBoolAsInt(p *bool) string {
	if p == nil {
		return "NULL"
	}
	if *p {
		return "1"
	}
	return "0"
}
