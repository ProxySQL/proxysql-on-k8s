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
		if strings.Contains(q, k) {
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
