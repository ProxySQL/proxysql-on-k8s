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
	"maps"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

// cnfTrue is the libconfig boolean literal ("true"), used for every
// cnf variable the operator renders as an on/off flag.
const cnfTrue = "true"

// reservedCnfKeys are rejected in validateCnfVars: never overridable via
// spec.variables. Two families:
//
//   - bootstrap-structural literals the template renders itself: credential
//     lines (admin_credentials, monitor_username/password,
//     cluster_username/password — values derived from the auth Secret) and
//     listener ifaces/interfaces lines (values derived from spec ports).
//   - keys owned by dedicated spec fields WITH StatefulSet coupling
//     (container ports, probe wiring): restapi_*/web_* toggles and ports —
//     overriding them in the cnf alone would desync the pod spec.
//
// Template-defaulted keys WITHOUT such coupling (mysql-threads,
// admin-cluster_check_*, eventslog_*) are NOT reserved: they render through
// the overlay-merge in BootstrapCnf, where a spec.variables value replaces
// the default (each key renders exactly once — libconfig rejects duplicate
// settings).
//
// NOTE: this is the OVERRIDE-rejection set only. Runtime-vs-restart
// classification (ParseCnfVariables / NormalizeCnf) uses the narrower
// structuralCnfKeys in cnf_variables.go — e.g. mysql-monitor_password is
// reserved against user override here, yet a spec.auth monitor rotation
// still runtime-applies restart-free (the documented monitor-credential
// exception).
var reservedCnfKeys = map[string]struct{}{
	"admin-admin_credentials": {},
	"admin-mysql_ifaces":      {},
	"mysql-interfaces":        {},
	"pgsql-interfaces":        {},
	"mysql-monitor_username":  {},
	"mysql-monitor_password":  {},
	"pgsql-monitor_username":  {},
	"pgsql-monitor_password":  {},
	"admin-cluster_username":  {},
	"admin-cluster_password":  {},
	"admin-restapi_enabled":   {},
	"admin-restapi_port":      {},
	"admin-web_enabled":       {},
	"admin-web_port":          {},

	// Backend TLS path variables (spec.tls.backend-owned; see tlsCnfVars
	// below): rendered by the operator with values fixed to the
	// backend-tls mount when spec.tls.backend is configured. Only the
	// RENDERED variables are reserved. The have_ssl flags are deliberately
	// NOT reserved: the operator never renders them (they default true in
	// 3.0), and a TLS-less cluster legitimately sets e.g.
	// mysql-have_ssl="false" via spec.variables to disable the
	// autogen-cert frontend TLS (runtime-settable, so the flip applies
	// restart-free). Likewise the unrendered p2s tuning knobs
	// (capath/cipher/crl/crlpath) stay user-settable — the operator never
	// renders them, so there is nothing to collide with.
	"mysql-ssl_p2s_ca":   {},
	"mysql-ssl_p2s_cert": {},
	"mysql-ssl_p2s_key":  {},
	"pgsql-ssl_p2s_ca":   {},
	"pgsql-ssl_p2s_cert": {},
	"pgsql-ssl_p2s_key":  {},
}

// tlsCnfVars — the complete TLS variable surface of ProxySQL 3.0, verified
// live on 2026-07-23 against the shipped proxysql/proxysql:3.0 image
// (3.0.9-618-g7ddb3dc, image ID 77bfbfc3d21c) by querying
// `SELECT variable_name FROM global_variables WHERE variable_name LIKE
// '%ssl%' OR ... '%tls%' OR ... '%cert%'` on the admin interface and
// cross-checking the full 403-variable dump (evidence:
// .superpowers/sdd/task-3-report.md):
//
//	mysql-have_ssl / pgsql-have_ssl                       frontend TLS enable, default true
//	mysql-ssl_p2s_{ca,capath,cert,cipher,crl,crlpath,key} backend (proxy-to-server)
//	pgsql-ssl_p2s_{ca,capath,cert,cipher,crl,crlpath,key} backend, pgsql equivalents
//	admin-ssl_keylog_file                                 debug TLS keylog only
//
// Crucially there are NO frontend/admin cert-path variables: the
// frontend/admin serving certs are the fixed datadir file names
// proxysql-{ca,cert,key}.pem (auto-generated when absent, re-read by
// `PROXYSQL RELOAD TLS`, live-confirmed via stats_tls_certificates and by
// `SET mysql-ssl_cert=...` → "Unknown global variable"). Frontend/admin
// cert delivery therefore does NOT go through the cnf at all — the
// StatefulSet's tls-init container symlinks the datadir names into the
// Secret mount (statefulset.go), boot-probe verified: proxysql loads the
// symlinked certs, serves them on 6033 AND 6032, and never clobbers the
// links. Only the backend ssl_p2s_* variables are rendered into the cnf.
var tlsCnfVars = map[string]string{
	"ssl_p2s_ca":   backendTLSMountPath + "/ca.crt",
	"ssl_p2s_cert": backendTLSMountPath + "/tls.crt",
	"ssl_p2s_key":  backendTLSMountPath + "/tls.key",
}

// cnfVarName constrains the variable name after its domain prefix. ProxySQL
// global variable names are lowercase snake_case; anything else is either a
// typo or an injection attempt.
var cnfVarName = regexp.MustCompile(`^[a-z0-9_]+$`)

// validateCnfVars rejects reserved keys, malformed names, and values that
// could break out of the double-quoted `name="value"` cnf rendering
// (quotes, backslashes, and control characters). Plain rejection — no
// escaping — keeps the rendered cnf trivially safe.
func validateCnfVars(vars map[string]string, prefix string) error {
	for k, v := range vars {
		if _, reserved := reservedCnfKeys[k]; reserved {
			return fmt.Errorf("spec.variables: %q is reserved (bootstrap-structural)", k)
		}
		if !cnfVarName.MatchString(strings.TrimPrefix(k, prefix)) {
			return fmt.Errorf("spec.variables: invalid variable name %q", k)
		}
		for _, r := range v {
			if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
				return fmt.Errorf("spec.variables: value for %q contains disallowed characters", k)
			}
		}
	}
	return nil
}

// bootstrapCnfTemplate is the minimal proxysql.cnf the operator writes into
// the bootstrap ConfigMap. It contains only what ProxySQL needs to start
// listening and accept admin-port writes: credentials, listening interfaces,
// monitor user, and (when replicas > 1) the proxysql_servers list for
// ProxySQL Cluster sync. Backends/users/query rules are pushed at runtime
// via ProxySQLConfig.
//
// Only credential lines and ifaces/interfaces listener lines are literal
// here. Every other per-section variable (threads, restapi_*/web_*,
// cluster_check_*/save/diffs, eventslog_*) comes in through the
// {Admin,MySQL,PgSQL}Extra slices, pre-merged with the user's spec.variables
// in BootstrapCnf so each key renders exactly once — libconfig treats a
// duplicated setting as a parse error, which crashloops the pod at boot.
const bootstrapCnfTemplate = `# Operator-managed bootstrap config for ProxySQLCluster {{ .ClusterName }}.
# Backends, users, query rules, and runtime tuning are pushed via ProxySQLConfig.
datadir="/var/lib/proxysql"

admin_variables=
{
  admin_credentials="admin:{{ .AdminPassword }};radmin:{{ .RadminPassword }}{{ if .ExtraAdminUser }};{{ .ExtraAdminUser }}:{{ .ExtraAdminPassword }}{{ end }}"
  mysql_ifaces="0.0.0.0:{{ .AdminPort }}"
{{- if .ClusterSync }}
  cluster_username="radmin"
  cluster_password="{{ .RadminPassword }}"
{{- end }}
{{- range .AdminExtra }}
  {{ .Name }}="{{ .Value }}"
{{- end }}
}

{{- if .MySQLEnabled }}

mysql_variables=
{
  interfaces="0.0.0.0:{{ .MySQLPort }}"
  monitor_username="monitor"
  monitor_password="{{ .MonitorPassword }}"
{{- range .MySQLExtra }}
  {{ .Name }}="{{ .Value }}"
{{- end }}
}
{{- end }}

{{- if .PostgreSQLEnabled }}

pgsql_variables=
{
  interfaces="0.0.0.0:{{ .PostgreSQLPort }}"
  monitor_username="monitor"
  monitor_password="{{ .MonitorPassword }}"
{{- range .PgSQLExtra }}
  {{ .Name }}="{{ .Value }}"
{{- end }}
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
	ClusterName        string
	AdminPassword      string
	RadminPassword     string
	MonitorPassword    string
	ExtraAdminUser     string
	ExtraAdminPassword string
	AdminPort          int32
	MySQLEnabled       bool
	MySQLPort          int32
	PostgreSQLEnabled  bool
	PostgreSQLPort     int32
	ClusterSync        bool
	ProxySQLServers    []string
	AdminExtra         []cnfVar
	MySQLExtra         []cnfVar
	PgSQLExtra         []cnfVar
}

// cnfVar is a single user-supplied global variable, already stripped of its
// domain prefix, ready to render as `Name="Value"`.
type cnfVar struct {
	Name  string
	Value string
}

// mergedCnfVars overlays user-supplied variables (full-name keys; the domain
// prefix is stripped) over the operator's per-section defaults (bare-name
// keys). The user value wins for overlapping keys, so every key renders
// exactly once; the result is sorted by bare name for deterministic
// rendering.
func mergedCnfVars(defaults, user map[string]string, prefix string) []cnfVar {
	merged := make(map[string]string, len(defaults)+len(user))
	maps.Copy(merged, defaults)
	for k, v := range user {
		merged[strings.TrimPrefix(k, prefix)] = v
	}
	if len(merged) == 0 {
		return nil
	}
	out := make([]cnfVar, 0, len(merged))
	for k, v := range merged {
		out = append(out, cnfVar{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// adminDefaultVars returns the admin_variables defaults the operator renders
// when spec.variables doesn't override them. restapi_*/web_* are reserved
// (spec-field-owned, STS-coupled) so they always carry the spec-derived
// values; the cluster_check_*/save/diffs tuning is overridable.
func (b *Builder) adminDefaultVars(clusterSync bool) map[string]string {
	d := map[string]string{}
	if isTrue(b.Spec.Metrics.Enabled) {
		d["restapi_enabled"] = cnfTrue
		d["restapi_port"] = fmt.Sprintf("%d", b.Spec.Metrics.Port)
	}
	if b.Spec.Protocols.Web.IsEnabled() {
		d["web_enabled"] = cnfTrue
		d["web_port"] = fmt.Sprintf("%d", b.Spec.Protocols.Web.Port)
	}
	if clusterSync {
		d["cluster_check_interval_ms"] = "200"
		d["cluster_check_status_frequency"] = "100"
		d["cluster_mysql_query_rules_save_to_disk"] = cnfTrue
		d["cluster_mysql_servers_save_to_disk"] = cnfTrue
		d["cluster_mysql_users_save_to_disk"] = cnfTrue
		d["cluster_proxysql_servers_save_to_disk"] = cnfTrue
		d["cluster_mysql_query_rules_diffs_before_sync"] = "3"
		d["cluster_mysql_servers_diffs_before_sync"] = "3"
		d["cluster_mysql_users_diffs_before_sync"] = "3"
		d["cluster_proxysql_servers_diffs_before_sync"] = "3"
	}
	return d
}

// mysqlDefaultVars returns the mysql_variables defaults (threads plus, with
// query logging enabled, the eventslog_* wiring for the fluent-bit sidecar,
// plus the reserved backend TLS paths when spec.tls.backend is configured).
func (b *Builder) mysqlDefaultVars() map[string]string {
	d := map[string]string{"threads": "4"}
	if b.LoggingEnabled() && b.Spec.Logging.QueryLog {
		d["eventslog_filename"] = "/var/log/proxysql/queries"
		d["eventslog_default_log"] = "1"
		d["eventslog_format"] = "2"
		d["eventslog_filesize"] = "52428800"
	}
	b.addBackendTLSVars(d)
	return d
}

// pgsqlDefaultVars returns the pgsql_variables defaults.
func (b *Builder) pgsqlDefaultVars() map[string]string {
	d := map[string]string{"threads": "4"}
	b.addBackendTLSVars(d)
	return d
}

// addBackendTLSVars adds the backend (proxy-to-server) TLS path variables
// to a section's defaults map. Identical for the mysql and pgsql sections
// (the verified 3.0 names differ only in their section prefix). Rendered
// only when spec.tls is enabled AND backend.caSecretName is set; the
// client cert/key pair additionally requires backend.clientCertSecretName.
// The values are fixed mount paths — cert ROTATION changes Secret content,
// never these lines, so rotation stays invisible to the cnf machinery.
// Although merged through the defaults map, these keys are reserved
// (reservedCnfKeys), so no user override can reach the merge.
func (b *Builder) addBackendTLSVars(d map[string]string) {
	if !b.backendTLSEnabled() {
		return
	}
	d["ssl_p2s_ca"] = tlsCnfVars["ssl_p2s_ca"]
	if b.Spec.TLS.Backend.ClientCertSecretName != "" {
		d["ssl_p2s_cert"] = tlsCnfVars["ssl_p2s_cert"]
		d["ssl_p2s_key"] = tlsCnfVars["ssl_p2s_key"]
	}
}

// BootstrapCnf renders the minimal proxysql.cnf for this cluster.
// proxysqlServers is the list of peer pod DNS names for ProxySQL Cluster sync
// (only populated when replicas > 1).
func (b *Builder) BootstrapCnf(proxysqlServers []string) (string, error) {
	for _, m := range []struct {
		vars   map[string]string
		prefix string
	}{
		{b.Spec.Variables.Admin, "admin-"},
		{b.Spec.Variables.MySQL, "mysql-"},
		{b.Spec.Variables.PostgreSQL, "pgsql-"},
	} {
		if err := validateCnfVars(m.vars, m.prefix); err != nil {
			return "", err
		}
	}
	tpl, err := template.New("proxysql.cnf").Parse(bootstrapCnfTemplate)
	if err != nil {
		return "", fmt.Errorf("parse cnf template: %w", err)
	}
	clusterSync := len(proxysqlServers) > 0
	data := cnfData{
		ClusterName:        b.Name(),
		AdminPassword:      b.Pw.Admin,
		RadminPassword:     b.Pw.Radmin,
		MonitorPassword:    b.Pw.Monitor,
		ExtraAdminUser:     b.Pw.ExtraAdminUser,
		ExtraAdminPassword: b.Pw.ExtraAdminPassword,
		AdminPort:          b.Spec.Protocols.Admin.Port,
		MySQLEnabled:       b.Spec.Protocols.MySQL.IsEnabled(),
		MySQLPort:          b.Spec.Protocols.MySQL.Port,
		PostgreSQLEnabled:  b.Spec.Protocols.PostgreSQL.IsEnabled(),
		PostgreSQLPort:     b.Spec.Protocols.PostgreSQL.Port,
		ClusterSync:        clusterSync,
		ProxySQLServers:    proxysqlServers,
		AdminExtra:         mergedCnfVars(b.adminDefaultVars(clusterSync), b.Spec.Variables.Admin, "admin-"),
		MySQLExtra:         mergedCnfVars(b.mysqlDefaultVars(), b.Spec.Variables.MySQL, "mysql-"),
		PgSQLExtra:         mergedCnfVars(b.pgsqlDefaultVars(), b.Spec.Variables.PostgreSQL, "pgsql-"),
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
