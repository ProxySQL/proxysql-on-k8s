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

package builders

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// bootstrapCnfTemplate is the minimal proxysql.cnf the operator writes into
// the bootstrap ConfigMap. It contains only what ProxySQL needs to start
// listening and accept admin-port writes: credentials, listening interfaces,
// monitor user, and (when replicas > 1) the proxysql_servers list for
// ProxySQL Cluster sync. Backends/users/query rules are pushed at runtime
// via ProxySQLConfig.
const bootstrapCnfTemplate = `# Operator-managed bootstrap config for ProxySQLCluster {{ .ClusterName }}.
# Backends, users, query rules, and runtime tuning are pushed via ProxySQLConfig.
datadir="/var/lib/proxysql"

admin_variables=
{
  admin_credentials="admin:{{ .AdminPassword }};radmin:{{ .RadminPassword }}"
  mysql_ifaces="0.0.0.0:{{ .AdminPort }}"
{{- if .MetricsEnabled }}
  restapi_enabled=true
  restapi_port={{ .MetricsPort }}
{{- end }}
{{- if .WebEnabled }}
  web_enabled=true
  web_port={{ .WebPort }}
{{- end }}
{{- if .ClusterSync }}
  cluster_username="radmin"
  cluster_password="{{ .RadminPassword }}"
  cluster_check_interval_ms=200
  cluster_check_status_frequency=100
  cluster_mysql_query_rules_save_to_disk=true
  cluster_mysql_servers_save_to_disk=true
  cluster_mysql_users_save_to_disk=true
  cluster_proxysql_servers_save_to_disk=true
  cluster_mysql_query_rules_diffs_before_sync=3
  cluster_mysql_servers_diffs_before_sync=3
  cluster_mysql_users_diffs_before_sync=3
  cluster_proxysql_servers_diffs_before_sync=3
{{- end }}
}

{{- if .MySQLEnabled }}

mysql_variables=
{
  interfaces="0.0.0.0:{{ .MySQLPort }}"
  monitor_username="monitor"
  monitor_password="{{ .MonitorPassword }}"
  threads=4
}
{{- end }}

{{- if .PostgreSQLEnabled }}

pgsql_variables=
{
  interfaces="0.0.0.0:{{ .PostgreSQLPort }}"
  monitor_username="monitor"
  monitor_password="{{ .MonitorPassword }}"
  threads=4
}
{{- end }}

{{- if .ProxySQLServers }}

proxysql_servers=
(
{{- range $i, $s := .ProxySQLServers }}
{{- if $i }},{{ end }}
  { hostname="{{ $s }}", port={{ $.AdminPort }}, weight=0, comment="" }
{{- end }}
)
{{- end }}
`

// cnfData is the input to bootstrapCnfTemplate.
type cnfData struct {
	ClusterName       string
	AdminPassword     string
	RadminPassword    string
	MonitorPassword   string
	AdminPort         int32
	MySQLEnabled      bool
	MySQLPort         int32
	PostgreSQLEnabled bool
	PostgreSQLPort    int32
	MetricsEnabled    bool
	MetricsPort       int32
	WebEnabled        bool
	WebPort           int32
	ClusterSync       bool
	ProxySQLServers   []string
}

// BootstrapCnf renders the minimal proxysql.cnf for this cluster.
// proxysqlServers is the list of peer pod DNS names for ProxySQL Cluster sync
// (only populated when replicas > 1).
func (b *Builder) BootstrapCnf(proxysqlServers []string) (string, error) {
	tpl, err := template.New("proxysql.cnf").Parse(bootstrapCnfTemplate)
	if err != nil {
		return "", fmt.Errorf("parse cnf template: %w", err)
	}
	data := cnfData{
		ClusterName:       b.Name(),
		AdminPassword:     b.Pw.Admin,
		RadminPassword:    b.Pw.Radmin,
		MonitorPassword:   b.Pw.Monitor,
		AdminPort:         b.Spec.Protocols.Admin.Port,
		MySQLEnabled:      b.Spec.Protocols.MySQL.Enabled,
		MySQLPort:         b.Spec.Protocols.MySQL.Port,
		PostgreSQLEnabled: b.Spec.Protocols.PostgreSQL.Enabled,
		PostgreSQLPort:    b.Spec.Protocols.PostgreSQL.Port,
		MetricsEnabled:    isTrue(b.Spec.Metrics.Enabled),
		MetricsPort:       b.Spec.Metrics.Port,
		WebEnabled:        b.Spec.Protocols.Web.Enabled,
		WebPort:           b.Spec.Protocols.Web.Port,
		ClusterSync:       len(proxysqlServers) > 0,
		ProxySQLServers:   proxysqlServers,
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render cnf: %w", err)
	}
	// Normalize line endings; the template's `{{- ... }}` trimming can leave
	// a trailing blank line on some platforms.
	return strings.TrimRight(buf.String(), "\n") + "\n", nil
}

// ProxySQLServerDNS returns the stable per-pod DNS names for the StatefulSet
// (used to populate proxysql_servers when replicas > 1).
func (b *Builder) ProxySQLServerDNS() []string {
	if b.Spec.Replicas == nil || *b.Spec.Replicas <= 1 {
		return nil
	}
	headless := b.HeadlessName()
	out := make([]string, 0, *b.Spec.Replicas)
	for i := int32(0); i < *b.Spec.Replicas; i++ {
		out = append(out, fmt.Sprintf("%s-%d.%s.%s.svc", b.Name(), i, headless, b.Namespace()))
	}
	return out
}
