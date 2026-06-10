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
// mysql_replication_hostgroups, pgsql_servers, pgsql_users, pgsql_query_rules,
// proxysql_servers, plus admin/mysql/pgsql variables.
type ProxySQLConfigSpec struct {
	// ClusterRef points to the ProxySQLCluster this config applies to.
	// Must exist in the same namespace.
	// +required
	ClusterRef corev1.LocalObjectReference `json:"clusterRef"`

	// MySQL backend topology.
	// +optional
	MySQLServers []MySQLServer `json:"mysqlServers,omitempty"`
	// +optional
	MySQLUsers []MySQLUser `json:"mysqlUsers,omitempty"`
	// +optional
	MySQLQueryRules []MySQLQueryRule `json:"mysqlQueryRules,omitempty"`
	// +optional
	MySQLReplicationHostgroups []MySQLReplicationHostgroup `json:"mysqlReplicationHostgroups,omitempty"`

	// PostgreSQL backend topology (ProxySQL 3.x).
	// +optional
	PostgreSQLServers []PostgreSQLServer `json:"pgsqlServers,omitempty"`
	// +optional
	PostgreSQLUsers []PostgreSQLUser `json:"pgsqlUsers,omitempty"`
	// +optional
	PostgreSQLQueryRules []PostgreSQLQueryRule `json:"pgsqlQueryRules,omitempty"`

	// ProxySQLServers identifies peer nodes for ProxySQL Cluster sync.
	// When empty, the operator auto-populates from the ProxySQLCluster's StatefulSet pods.
	// +optional
	ProxySQLServers []ProxySQLServerEntry `json:"proxysqlServers,omitempty"`

	// ProxySQL variable overrides. Pushed via admin SQL UPDATE.
	// +optional
	AdminVariables map[string]string `json:"adminVariables,omitempty"`
	// +optional
	MySQLVariables map[string]string `json:"mysqlVariables,omitempty"`
	// +optional
	PostgreSQLVariables map[string]string `json:"pgsqlVariables,omitempty"`
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

// PostgreSQLQueryRule maps to pgsql_query_rules.
type PostgreSQLQueryRule struct {
	RuleID int32 `json:"ruleId"`
	// +optional
	Active *bool `json:"active,omitempty"`
	// +optional
	MatchPattern string `json:"matchPattern,omitempty"`
	// +optional
	DestinationHostgroup *int32 `json:"destinationHostgroup,omitempty"`
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
