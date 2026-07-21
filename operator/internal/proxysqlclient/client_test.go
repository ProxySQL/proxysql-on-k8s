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
	"strings"
	"testing"
)

// TestSnippet_RedactsQuotedLiterals ensures secret literals embedded in
// single-quoted SQL string literals never make it into the snippet used for
// error messages (which controller-runtime logs and which can surface in
// status conditions), while the verb + table name stay intact for
// debuggability.
func TestSnippet_RedactsQuotedLiterals(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "update global_variables monitor password",
			query:       `UPDATE global_variables SET variable_value='s3cr3t-monitor-pw' WHERE variable_name='mysql-monitor_password'`,
			wantContain: []string{"UPDATE global_variables SET variable_value=", "WHERE variable_name="},
			wantAbsent:  []string{"s3cr3t-monitor-pw", "mysql-monitor_password"},
		},
		{
			name:        "insert mysql_users password",
			query:       `INSERT INTO mysql_users (username,password) VALUES ('app_user','hunter2pw')`,
			wantContain: []string{"INSERT INTO mysql_users"},
			wantAbsent:  []string{"hunter2pw"},
		},
		{
			name:        "doubled single-quote escape inside literal is not an early terminator",
			query:       `UPDATE mysql_users SET password='it''s-a-secret' WHERE username='app_user'`,
			wantContain: []string{"UPDATE mysql_users SET password="},
			wantAbsent:  []string{"it''s-a-secret", "it", "a-secret"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snippet(tt.query)
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("snippet(%q) = %q, want it to contain %q", tt.query, got, want)
				}
			}
			for _, bad := range tt.wantAbsent {
				if strings.Contains(got, bad) {
					t.Errorf("snippet(%q) = %q, must not contain secret literal %q", tt.query, got, bad)
				}
			}
			if !strings.Contains(got, "***") {
				t.Errorf("snippet(%q) = %q, want masked literal marker %q", tt.query, got, "***")
			}
		})
	}
}

// TestSnippet_LongQueryStillTruncatesAfterRedaction verifies the existing
// truncation behavior for long queries composes correctly with redaction:
// the literal is masked before the length is measured/truncated.
func TestSnippet_LongQueryStillTruncatesAfterRedaction(t *testing.T) {
	long := "UPDATE global_variables SET variable_value='" + strings.Repeat("x", 500) + "' WHERE variable_name='mysql-monitor_password'"
	got := snippet(long)
	if strings.Contains(got, strings.Repeat("x", 20)) {
		t.Errorf("snippet(long) = %q, secret literal leaked", got)
	}
	if !strings.Contains(got, "UPDATE global_variables SET variable_value=") {
		t.Errorf("snippet(long) = %q, lost leading verb+table", got)
	}
}

// TestExecErrorMessage_RedactsSecretLiteral is a regression test for the
// error message shape produced by Exec: it must retain the query's leading
// verb + table for debuggability but must never leak a quoted secret
// literal. Exec itself requires a live DB connection to fail against, so
// this test exercises the same wrapping shape directly via snippet, mirroring
// exactly what Exec embeds in its error (see client.go: fmt.Errorf("%s: exec
// %q: %w", c.addr, snippet(query), err)).
func TestExecErrorMessage_RedactsSecretLiteral(t *testing.T) {
	query := `UPDATE global_variables SET variable_value='monitor-secret-pw' WHERE variable_name='mysql-monitor_password'`
	addr := "proxysql-0.proxysql:6032"
	errMsg := addr + ": exec " + quoteForTest(snippet(query)) + ": connection refused"

	if strings.Contains(errMsg, "monitor-secret-pw") {
		t.Fatalf("Exec-shaped error message leaked secret literal: %q", errMsg)
	}
	if !strings.Contains(errMsg, "UPDATE global_variables SET variable_value=") {
		t.Fatalf("Exec-shaped error message lost leading verb+table: %q", errMsg)
	}
	if !strings.Contains(errMsg, "WHERE variable_name=") {
		t.Fatalf("Exec-shaped error message lost WHERE clause shape: %q", errMsg)
	}
}

func quoteForTest(s string) string {
	return "\"" + s + "\""
}
