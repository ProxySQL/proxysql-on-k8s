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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProxySQLConfigSpec is the declarative ProxySQL configuration the operator
// pushes to a target ProxySQLCluster via its admin port. Fields map 1:1 to
// the ProxySQL admin tables: mysql_servers, mysql_users, mysql_query_rules,
// mysql_replication_hostgroups, mysql_hostgroup_attributes, pgsql_servers,
// pgsql_users, pgsql_query_rules, proxysql_servers, plus admin/mysql/pgsql
// variables.
type ProxySQLConfigSpec struct {
	// ClusterRef points to the ProxySQLCluster this config applies to.
	// Must exist in the same namespace.
	// +required
	ClusterRef corev1.LocalObjectReference `json:"clusterRef"`

	// MySQL backend topology.
	// +optional
	// +listType=map
	// +listMapKey=hostgroup
	// +listMapKey=hostname
	// +listMapKey=port
	MySQLServers []MySQLServer `json:"mysqlServers,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=username
	MySQLUsers []MySQLUser `json:"mysqlUsers,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=ruleId
	MySQLQueryRules []MySQLQueryRule `json:"mysqlQueryRules,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=writerHostgroup
	MySQLReplicationHostgroups []MySQLReplicationHostgroup `json:"mysqlReplicationHostgroups,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=hostgroup
	MySQLHostgroupAttributes []MySQLHostgroupAttributes `json:"mysqlHostgroupAttributes,omitempty"`

	// PostgreSQL backend topology (ProxySQL 3.x).
	// +optional
	// +listType=map
	// +listMapKey=hostgroup
	// +listMapKey=hostname
	// +listMapKey=port
	PostgreSQLServers []PostgreSQLServer `json:"pgsqlServers,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=username
	PostgreSQLUsers []PostgreSQLUser `json:"pgsqlUsers,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=ruleId
	PostgreSQLQueryRules []PostgreSQLQueryRule `json:"pgsqlQueryRules,omitempty"`

	// ProxySQLServers identifies peer nodes for ProxySQL Cluster sync.
	// When empty, the operator auto-populates from the ProxySQLCluster's StatefulSet pods.
	// +optional
	// +listType=map
	// +listMapKey=hostname
	// +listMapKey=port
	ProxySQLServers []ProxySQLServerEntry `json:"proxysqlServers,omitempty"`

	// ProxySQL variable overrides. Pushed via admin SQL UPDATE.
	// +optional
	AdminVariables map[string]string `json:"adminVariables,omitempty"`
	// +optional
	MySQLVariables map[string]string `json:"mysqlVariables,omitempty"`
	// +optional
	PostgreSQLVariables map[string]string `json:"pgsqlVariables,omitempty"`

	// SQLStatements is raw admin SQL executed verbatim on every replica,
	// in order, after all structured config, on EVERY sync pass — including
	// on new or restarted replicas and after drift resyncs. Statements MUST
	// be idempotent. They are opaque to the operator: no implicit
	// LOAD/SAVE is appended, their effects are not drift-tracked, and
	// deletion cleanup does not reverse them. A statement that breaks admin
	// connectivity (e.g. changing admin credentials) locks the operator out
	// until a pod restart restores the cnf-based credentials.
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	SQLStatements []string `json:"sqlStatements,omitempty"`
}

// MySQLServer maps to a row in the ProxySQL mysql_servers table.
type MySQLServer struct {
	// Hostgroup is the mysql_servers.hostgroup_id.
	Hostgroup int32 `json:"hostgroup"`
	// Hostname of the backend MySQL server.
	Hostname string `json:"hostname"`
	// Port defaults to 3306.
	// +optional
	// +kubebuilder:default=3306
	Port int32 `json:"port,omitempty"`
	// +optional
	Weight *int32 `json:"weight,omitempty"`
	// +optional
	MaxConnections *int32 `json:"maxConnections,omitempty"`
	// +optional
	MaxReplicationLag *int32 `json:"maxReplicationLag,omitempty"`
	// +optional
	UseSSL *bool `json:"useSSL,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// MySQLUser maps to a row in the ProxySQL mysql_users table.
type MySQLUser struct {
	Username string `json:"username"`
	// PasswordSecretRef references a Secret holding the user's password.
	PasswordSecretRef corev1.SecretKeySelector `json:"passwordSecretRef"`
	// +optional
	// +kubebuilder:default=0
	DefaultHostgroup int32 `json:"defaultHostgroup,omitempty"`
	// +optional
	Active *bool `json:"active,omitempty"`
	// +optional
	MaxConnections *int32 `json:"maxConnections,omitempty"`
	// +optional
	UseSSL *bool `json:"useSSL,omitempty"`
	// +optional
	DefaultSchema string `json:"defaultSchema,omitempty"`
	// +optional
	TransactionPersistent *bool `json:"transactionPersistent,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// MySQLQueryRule maps to a row in the ProxySQL mysql_query_rules table.
type MySQLQueryRule struct {
	RuleID int32 `json:"ruleId"`
	// +optional
	Active *bool `json:"active,omitempty"`
	// +optional
	Username string `json:"username,omitempty"`
	// +optional
	SchemaName string `json:"schemaName,omitempty"`
	// +optional
	MatchPattern string `json:"matchPattern,omitempty"`
	// +optional
	MatchDigest string `json:"matchDigest,omitempty"`
	// +optional
	DestinationHostgroup *int32 `json:"destinationHostgroup,omitempty"`
	// ReplacePattern is the replacement text applied to queries matching
	// matchPattern (query rewriting). ProxySQL has a single replace_pattern
	// column: matchPattern selects the text to replace, replacePattern is
	// what it is rewritten to (RE2/PCRE backreferences like \1 supported).
	// Unset means no rewriting.
	// +optional
	ReplacePattern string `json:"replacePattern,omitempty"`
	// MirrorHostgroup additionally sends a copy of matching queries to this
	// hostgroup (query mirroring). Maps to mirror_hostgroup; unset = no mirroring.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MirrorHostgroup *int32 `json:"mirrorHostgroup,omitempty"`
	// Timeout in milliseconds for matching queries; queries running longer
	// are killed. Maps to timeout; unset = mysql-default_query_timeout.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Timeout *int32 `json:"timeout,omitempty"`
	// Delay in milliseconds applied to matching queries (throttling).
	// Maps to delay; unset = no delay.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Delay *int32 `json:"delay,omitempty"`
	// ErrorMessage blocks matching queries and returns this message to the
	// client instead of executing them (query firewalling). Maps to error_msg.
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`
	// FlagIn assigns this rule to a chaining flag: the rule is only evaluated
	// for queries whose current flag equals flagIn. Defaults to 0, the entry
	// point of the rule chain. Maps to flagIN.
	// +optional
	// +kubebuilder:validation:Minimum=0
	FlagIn *int32 `json:"flagIn,omitempty"`
	// FlagOut sets the flag used to evaluate subsequent rules when this rule
	// matches (rule chaining). Maps to flagOUT; unset = keep current flag.
	// +optional
	// +kubebuilder:validation:Minimum=0
	FlagOut *int32 `json:"flagOut,omitempty"`
	// Log enables query logging for matching queries. Maps to log; unset
	// inherits ProxySQL's default behavior.
	// +optional
	Log *bool `json:"log,omitempty"`
	// CacheTTL in milliseconds enables the query cache for matching queries:
	// resultsets are cached and served for this long. Maps to cache_ttl;
	// unset = no caching.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CacheTTL *int32 `json:"cacheTTL,omitempty"`
	// CacheEmptyResult controls whether empty resultsets are cached too.
	// Only meaningful together with cacheTTL. Maps to cache_empty_result.
	// +optional
	CacheEmptyResult *bool `json:"cacheEmptyResult,omitempty"`
	// +optional
	Apply *bool `json:"apply,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// MySQLReplicationHostgroup maps to mysql_replication_hostgroups.
type MySQLReplicationHostgroup struct {
	WriterHostgroup int32 `json:"writerHostgroup"`
	ReaderHostgroup int32 `json:"readerHostgroup"`
	// +optional
	// +kubebuilder:default=read_only
	// +kubebuilder:validation:Enum=read_only;innodb_read_only;super_read_only;read_only|innodb_read_only;read_only&innodb_read_only
	CheckType string `json:"checkType,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// MySQLHostgroupAttributes maps to a row in mysql_hostgroup_attributes,
// per-hostgroup connection-handling behavior. Unset optional fields fall back
// to ProxySQL's column defaults.
type MySQLHostgroupAttributes struct {
	// Hostgroup is the mysql_hostgroup_attributes.hostgroup_id this row applies to.
	// +kubebuilder:validation:Minimum=0
	Hostgroup int32 `json:"hostgroup"`
	// MaxNumOnlineServers caps how many servers in the hostgroup are treated
	// as ONLINE. Maps to max_num_online_servers; ProxySQL default 1000000.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MaxNumOnlineServers *int32 `json:"maxNumOnlineServers,omitempty"`
	// Autocommit enforces autocommit on backend connections of this hostgroup:
	// -1 = don't enforce (ProxySQL default), 0 = force off, 1 = force on.
	// +optional
	// +kubebuilder:validation:Enum=-1;0;1
	Autocommit *int32 `json:"autocommit,omitempty"`
	// FreeConnectionsPct is the percentage of mysql-max_connections kept open
	// to the hostgroup as warm free connections. Maps to free_connections_pct;
	// ProxySQL default 10.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FreeConnectionsPct *int32 `json:"freeConnectionsPct,omitempty"`
	// InitConnect is SQL executed on every new backend connection to this
	// hostgroup, overriding mysql-init_connect. Maps to init_connect.
	// +optional
	InitConnect string `json:"initConnect,omitempty"`
	// Multiplex enables/disables connection multiplexing for the hostgroup.
	// Maps to multiplex; ProxySQL default true.
	// +optional
	Multiplex *bool `json:"multiplex,omitempty"`
	// ConnectionWarming pre-opens free connections to reach
	// freeConnectionsPct before serving traffic. Maps to connection_warming;
	// ProxySQL default false.
	// +optional
	ConnectionWarming *bool `json:"connectionWarming,omitempty"`
	// ThrottleConnectionsPerSec caps new backend connections per second to
	// the hostgroup. Maps to throttle_connections_per_sec; ProxySQL default
	// 1000000.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	ThrottleConnectionsPerSec *int32 `json:"throttleConnectionsPerSec,omitempty"`
	// IgnoreSessionVariables is a JSON array of session variable names ProxySQL
	// must not track for this hostgroup, e.g. ["sql_log_bin"]. Maps to
	// ignore_session_variables; must be valid JSON (or unset).
	// +optional
	IgnoreSessionVariables string `json:"ignoreSessionVariables,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// PostgreSQLServer maps to pgsql_servers (ProxySQL 3.x).
type PostgreSQLServer struct {
	Hostgroup int32  `json:"hostgroup"`
	Hostname  string `json:"hostname"`
	// +optional
	// +kubebuilder:default=5432
	Port int32 `json:"port,omitempty"`
	// +optional
	Weight *int32 `json:"weight,omitempty"`
	// +optional
	MaxConnections *int32 `json:"maxConnections,omitempty"`
	// +optional
	UseSSL *bool `json:"useSSL,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// PostgreSQLUser maps to pgsql_users.
type PostgreSQLUser struct {
	Username          string                   `json:"username"`
	PasswordSecretRef corev1.SecretKeySelector `json:"passwordSecretRef"`
	// +optional
	DefaultHostgroup int32 `json:"defaultHostgroup,omitempty"`
	// +optional
	Active *bool `json:"active,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// PostgreSQLQueryRule maps to pgsql_query_rules. ProxySQL 3.x pgsql_query_rules
// carries the same rewriting/mirroring/caching/chaining columns as
// mysql_query_rules, so the optional fields below mirror MySQLQueryRule.
type PostgreSQLQueryRule struct {
	RuleID int32 `json:"ruleId"`
	// +optional
	Active *bool `json:"active,omitempty"`
	// +optional
	MatchPattern string `json:"matchPattern,omitempty"`
	// +optional
	DestinationHostgroup *int32 `json:"destinationHostgroup,omitempty"`
	// ReplacePattern is the replacement text applied to queries matching
	// matchPattern (query rewriting). ProxySQL has a single replace_pattern
	// column: matchPattern selects the text to replace, replacePattern is
	// what it is rewritten to. Unset means no rewriting.
	// +optional
	ReplacePattern string `json:"replacePattern,omitempty"`
	// MirrorHostgroup additionally sends a copy of matching queries to this
	// hostgroup (query mirroring). Maps to mirror_hostgroup; unset = no mirroring.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MirrorHostgroup *int32 `json:"mirrorHostgroup,omitempty"`
	// Timeout in milliseconds for matching queries; queries running longer
	// are killed. Maps to timeout; unset = global default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Timeout *int32 `json:"timeout,omitempty"`
	// Delay in milliseconds applied to matching queries (throttling).
	// Maps to delay; unset = no delay.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Delay *int32 `json:"delay,omitempty"`
	// ErrorMessage blocks matching queries and returns this message to the
	// client instead of executing them (query firewalling). Maps to error_msg.
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`
	// FlagIn assigns this rule to a chaining flag: the rule is only evaluated
	// for queries whose current flag equals flagIn. Defaults to 0, the entry
	// point of the rule chain. Maps to flagIN.
	// +optional
	// +kubebuilder:validation:Minimum=0
	FlagIn *int32 `json:"flagIn,omitempty"`
	// FlagOut sets the flag used to evaluate subsequent rules when this rule
	// matches (rule chaining). Maps to flagOUT; unset = keep current flag.
	// +optional
	// +kubebuilder:validation:Minimum=0
	FlagOut *int32 `json:"flagOut,omitempty"`
	// Log enables query logging for matching queries. Maps to log; unset
	// inherits ProxySQL's default behavior.
	// +optional
	Log *bool `json:"log,omitempty"`
	// CacheTTL in milliseconds enables the query cache for matching queries:
	// resultsets are cached and served for this long. Maps to cache_ttl;
	// unset = no caching.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CacheTTL *int32 `json:"cacheTTL,omitempty"`
	// CacheEmptyResult controls whether empty resultsets are cached too.
	// Only meaningful together with cacheTTL. Maps to cache_empty_result.
	// +optional
	CacheEmptyResult *bool `json:"cacheEmptyResult,omitempty"`
	// +optional
	Apply *bool `json:"apply,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// ProxySQLServerEntry maps to a row in proxysql_servers.
type ProxySQLServerEntry struct {
	Hostname string `json:"hostname"`
	// +optional
	// +kubebuilder:default=6032
	Port int32 `json:"port,omitempty"`
	// +optional
	Weight int32 `json:"weight,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// ProxySQLConfigStatus reports the operator's view of config sync.
type ProxySQLConfigStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastAppliedHash is a SHA of the spec last successfully pushed to the cluster.
	// +optional
	LastAppliedHash string `json:"lastAppliedHash,omitempty"`

	// LastSyncTime is when the operator last asserted desired state on the
	// cluster — either by writing it, or by verifying via runtime read-back
	// that no replica had drifted.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// SyncedReplicas is the number of ProxySQL pods that received the latest config.
	// +optional
	SyncedReplicas int32 `json:"syncedReplicas,omitempty"`

	// DriftedReplicas is the number of ready replicas whose runtime tables
	// diverged from the desired config at the last runtime check. 0 when
	// everything converged.
	// +optional
	DriftedReplicas int32 `json:"driftedReplicas,omitempty"`

	// ShunnedBackends is the total number of backend server rows in SHUNNED
	// state across all replicas at the last runtime check.
	// +optional
	ShunnedBackends int32 `json:"shunnedBackends,omitempty"`

	// LastRuntimeCheckTime is when the operator last read runtime state back
	// from the replicas.
	// +optional
	LastRuntimeCheckTime *metav1.Time `json:"lastRuntimeCheckTime,omitempty"`

	// Conditions follow standard K8s conventions:
	//   Ready, Progressing, Degraded, ClusterFound
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pxcfg
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Synced",type=integer,JSONPath=`.status.syncedReplicas`
// +kubebuilder:printcolumn:name="Drifted",type=integer,JSONPath=`.status.driftedReplicas`
// +kubebuilder:printcolumn:name="Last-Sync",type=date,JSONPath=`.status.lastSyncTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProxySQLConfig is the Schema for the proxysqlconfigs API.
type ProxySQLConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ProxySQLConfig
	// +required
	Spec ProxySQLConfigSpec `json:"spec"`

	// status defines the observed state of ProxySQLConfig
	// +optional
	Status ProxySQLConfigStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ProxySQLConfigList contains a list of ProxySQLConfig.
type ProxySQLConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ProxySQLConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProxySQLConfig{}, &ProxySQLConfigList{})
}
