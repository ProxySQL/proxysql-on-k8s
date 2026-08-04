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
	"maps"
	"slices"
	"sort"
	"strconv"
)

// Union merges several Desired states — one per ProxySQLConfig targeting the same
// cluster — into a single desired runtime (#956). Without this the operator would treat
// each ProxySQLConfig as authoritative for the whole runtime, so two configs for one
// cluster oscillate (each Sync DELETEs the other's rows and re-LOADs its own).
//
// Sections are combined per key with LAST-WRITER-WINS: an entry from a later element of
// `desireds` overrides an earlier one with the same key. Callers must order `desireds`
// deterministically (the controller sorts by ProxySQLConfig name) so the outcome is stable
// and every config for the cluster reconciles to the SAME desired state.
//
// Output slices are emitted sorted by key so the fingerprint taken over the union does not
// flap between reconciles. Empty sections stay nil (not empty slices/maps).
//
// Merge keys: mysql/pgsql servers by "hostgroup:hostname:port"; users by username; query
// rules by rule id; replication hostgroups by writer hostgroup; hostgroup attributes by
// hostgroup; proxysql servers by "hostname:port"; the three variable maps by variable name.
// sqlStatements are concatenated in input order (opaque, idempotent).
func Union(desireds []*Desired) *Desired {
	msrv := map[string]MySQLServer{}
	musr := map[string]MySQLUser{}
	mrule := map[int32]MySQLQueryRule{}
	mrepl := map[int32]MySQLReplicationHostgroup{}
	mattr := map[int32]MySQLHostgroupAttributes{}
	pgsrv := map[string]PostgreSQLServer{}
	pgusr := map[string]PostgreSQLUser{}
	pgrule := map[int32]PostgreSQLQueryRule{}
	psrv := map[string]ProxySQLServer{}
	adminVars := map[string]string{}
	mysqlVars := map[string]string{}
	pgVars := map[string]string{}

	out := &Desired{}
	for _, d := range desireds {
		if d == nil {
			continue
		}
		for _, s := range d.MySQLServers {
			msrv[hostKey(s.Hostgroup, s.Hostname, s.Port)] = s
		}
		for _, u := range d.MySQLUsers {
			musr[u.Username] = u
		}
		for _, r := range d.MySQLQueryRules {
			mrule[r.RuleID] = r
		}
		for _, h := range d.MySQLReplicationHostgroups {
			mrepl[h.WriterHostgroup] = h
		}
		for _, a := range d.MySQLHostgroupAttributes {
			mattr[a.Hostgroup] = a
		}
		for _, s := range d.PostgreSQLServers {
			pgsrv[hostKey(s.Hostgroup, s.Hostname, s.Port)] = s
		}
		for _, u := range d.PostgreSQLUsers {
			pgusr[u.Username] = u
		}
		for _, r := range d.PostgreSQLQueryRules {
			pgrule[r.RuleID] = r
		}
		for _, s := range d.ProxySQLServers {
			psrv[s.Hostname+":"+strconv.Itoa(int(s.Port))] = s
		}
		maps.Copy(adminVars, d.AdminVariables)
		maps.Copy(mysqlVars, d.MySQLVariables)
		maps.Copy(pgVars, d.PostgreSQLVariables)
		out.SQLStatements = append(out.SQLStatements, d.SQLStatements...)
	}

	// Flatten each keyed map into a slice sorted by key for a stable fingerprint.
	for _, k := range sortedStrKeys(msrv) {
		out.MySQLServers = append(out.MySQLServers, msrv[k])
	}
	for _, k := range sortedStrKeys(musr) {
		out.MySQLUsers = append(out.MySQLUsers, musr[k])
	}
	for _, k := range sortedInt32Keys(mrule) {
		out.MySQLQueryRules = append(out.MySQLQueryRules, mrule[k])
	}
	for _, k := range sortedInt32Keys(mrepl) {
		out.MySQLReplicationHostgroups = append(out.MySQLReplicationHostgroups, mrepl[k])
	}
	for _, k := range sortedInt32Keys(mattr) {
		out.MySQLHostgroupAttributes = append(out.MySQLHostgroupAttributes, mattr[k])
	}
	for _, k := range sortedStrKeys(pgsrv) {
		out.PostgreSQLServers = append(out.PostgreSQLServers, pgsrv[k])
	}
	for _, k := range sortedStrKeys(pgusr) {
		out.PostgreSQLUsers = append(out.PostgreSQLUsers, pgusr[k])
	}
	for _, k := range sortedInt32Keys(pgrule) {
		out.PostgreSQLQueryRules = append(out.PostgreSQLQueryRules, pgrule[k])
	}
	for _, k := range sortedStrKeys(psrv) {
		out.ProxySQLServers = append(out.ProxySQLServers, psrv[k])
	}
	out.AdminVariables = nilIfEmpty(adminVars)
	out.MySQLVariables = nilIfEmpty(mysqlVars)
	out.PostgreSQLVariables = nilIfEmpty(pgVars)
	return out
}

func hostKey(hg int32, host string, port int32) string {
	return strconv.Itoa(int(hg)) + ":" + host + ":" + strconv.Itoa(int(port))
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func sortedStrKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedInt32Keys[V any](m map[int32]V) []int32 {
	ks := make([]int32, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
