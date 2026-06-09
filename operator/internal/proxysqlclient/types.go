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

	PostgreSQLServers    []PostgreSQLServer
	PostgreSQLUsers      []PostgreSQLUser
	PostgreSQLQueryRules []PostgreSQLQueryRule

	ProxySQLServers []ProxySQLServer

	AdminVariables      map[string]string
	MySQLVariables      map[string]string
	PostgreSQLVariables map[string]string
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
	MatchPattern         string
	MatchDigest          string
	DestinationHostgroup *int32
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
type PostgreSQLQueryRule struct {
	RuleID               int32
	Active               *bool
	MatchPattern         string
	DestinationHostgroup *int32
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
