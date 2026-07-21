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

// Package proxysqlclient is the thin SQL client the operator uses to push
// ProxySQLConfig to a ProxySQL admin port. It deals only in resolved values
// (passwords already pulled out of Secrets); the controller is responsible
// for the K8s-side resolution.
package proxysqlclient

// Desired is the resolved, password-substituted view of a ProxySQLConfig.
// One per ProxySQLConfig per reconcile.
type Desired struct {
	MySQLServers               []MySQLServer
	MySQLUsers                 []MySQLUser
	MySQLQueryRules            []MySQLQueryRule
	MySQLReplicationHostgroups []MySQLReplicationHostgroup
	MySQLHostgroupAttributes   []MySQLHostgroupAttributes

	PostgreSQLServers    []PostgreSQLServer
	PostgreSQLUsers      []PostgreSQLUser
	PostgreSQLQueryRules []PostgreSQLQueryRule

	ProxySQLServers []ProxySQLServer

	AdminVariables      map[string]string
	MySQLVariables      map[string]string
	PostgreSQLVariables map[string]string

	// SQLStatements is raw admin SQL executed verbatim after all
	// structured sections. Opaque: no implicit LOAD/SAVE, not drift-tracked.
	SQLStatements []string
}

// MySQLServer is the resolved form of a mysql_servers row.
type MySQLServer struct {
	Hostgroup         int32
	Hostname          string
	Port              int32
	Weight            *int32
	MaxConnections    *int32
	MaxReplicationLag *int32
	UseSSL            *bool
	Comment           string
}

// MySQLUser is the resolved form of a mysql_users row.
// Password is already pulled out of the referenced Secret.
type MySQLUser struct {
	Username              string
	Password              string
	DefaultHostgroup      int32
	Active                *bool
	MaxConnections        *int32
	UseSSL                *bool
	DefaultSchema         string
	TransactionPersistent *bool
	Comment               string
}

// MySQLQueryRule is the resolved form of a mysql_query_rules row.
type MySQLQueryRule struct {
	RuleID               int32
	Active               *bool
	Username             string
	SchemaName           string
	FlagIn               *int32 // flagIN: NOT NULL DEFAULT 0
	MatchPattern         string
	MatchDigest          string
	FlagOut              *int32 // flagOUT: nullable
	ReplacePattern       string // replace_pattern: nullable, "" renders as NULL
	DestinationHostgroup *int32
	CacheTTL             *int32 // cache_ttl (ms): nullable
	CacheEmptyResult     *bool  // cache_empty_result: nullable
	Timeout              *int32 // timeout (ms): nullable
	Delay                *int32 // delay (ms): nullable
	MirrorHostgroup      *int32 // mirror_hostgroup: nullable
	ErrorMessage         string // error_msg: nullable, "" renders as NULL
	Log                  *bool  // log: nullable
	Apply                *bool
	Comment              string
}

// MySQLReplicationHostgroup is the resolved form of a mysql_replication_hostgroups row.
type MySQLReplicationHostgroup struct {
	WriterHostgroup int32
	ReaderHostgroup int32
	CheckType       string
	Comment         string
}

// MySQLHostgroupAttributes is the resolved form of a mysql_hostgroup_attributes
// row. Every column in the ProxySQL 3.0 table is NOT NULL with a default, so
// unset pointer fields render the column default rather than NULL.
type MySQLHostgroupAttributes struct {
	Hostgroup                 int32
	MaxNumOnlineServers       *int32
	Autocommit                *int32
	FreeConnectionsPct        *int32
	InitConnect               string
	Multiplex                 *bool
	ConnectionWarming         *bool
	ThrottleConnectionsPerSec *int32
	IgnoreSessionVariables    string
	Comment                   string
}

// PostgreSQLServer is the resolved form of a pgsql_servers row.
type PostgreSQLServer struct {
	Hostgroup      int32
	Hostname       string
	Port           int32
	Weight         *int32
	MaxConnections *int32
	Comment        string
}

// PostgreSQLUser is the resolved form of a pgsql_users row.
type PostgreSQLUser struct {
	Username         string
	Password         string
	DefaultHostgroup int32
	Active           *bool
	Comment          string
}

// PostgreSQLQueryRule is the resolved form of a pgsql_query_rules row.
// ProxySQL 3.x pgsql_query_rules has the same rewriting/mirroring/caching/
// chaining columns as mysql_query_rules.
type PostgreSQLQueryRule struct {
	RuleID               int32
	Active               *bool
	FlagIn               *int32 // flagIN: NOT NULL DEFAULT 0
	MatchPattern         string
	FlagOut              *int32 // flagOUT: nullable
	ReplacePattern       string // replace_pattern: nullable, "" renders as NULL
	DestinationHostgroup *int32
	CacheTTL             *int32 // cache_ttl (ms): nullable
	CacheEmptyResult     *bool  // cache_empty_result: nullable
	Timeout              *int32 // timeout (ms): nullable
	Delay                *int32 // delay (ms): nullable
	MirrorHostgroup      *int32 // mirror_hostgroup: nullable
	ErrorMessage         string // error_msg: nullable, "" renders as NULL
	Log                  *bool  // log: nullable
	Apply                *bool
	Comment              string
}

// ProxySQLServer is the resolved form of a proxysql_servers row.
type ProxySQLServer struct {
	Hostname string
	Port     int32
	Weight   int32
	Comment  string
}
