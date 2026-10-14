// Package rdsconfig adapts the SaaS RDS-compatible document to the AWS
// plugin's managed bootstrap and query-operation contracts.
package rdsconfig

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	WriterHostgroup        int32 = 1
	ReaderHostgroup        int32 = 2
	engineFamilyMySQL            = "MYSQL"
	defaultTargetGroupName       = "default"
)

// Credentials are the operator-owned SigV4 credentials installed into one
// ProxySQL cluster. They are not customer AWS credentials.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

type document struct {
	SchemaVersion   int             `json:"schema_version"`
	Proxy           proxySettings   `json:"proxy"`
	TargetGroup     targetGroup     `json:"target_group"`
	Targets         []target        `json:"targets"`
	Tags            []tag           `json:"tags"`
	DefaultEndpoint defaultEndpoint `json:"default_endpoint"`
}

type proxySettings struct {
	ARN                         string   `json:"arn"`
	EngineFamily                string   `json:"engine_family"`
	Auth                        []auth   `json:"auth"`
	DefaultAuthScheme           string   `json:"default_auth_scheme"`
	DebugLogging                bool     `json:"debug_logging"`
	IdleClientTimeout           int      `json:"idle_client_timeout"`
	RequireTLS                  bool     `json:"require_tls"`
	EndpointNetworkType         string   `json:"endpoint_network_type"`
	TargetConnectionNetworkType string   `json:"target_connection_network_type"`
	VpcSubnetIDs                []string `json:"vpc_subnet_ids"`
	VpcSecurityGroupIDs         []string `json:"vpc_security_group_ids"`
}

type auth struct {
	AuthScheme             string `json:"auth_scheme,omitempty"`
	ClientPasswordAuthType string `json:"client_password_auth_type,omitempty"`
	Description            string `json:"description,omitempty"`
	IAMAuth                string `json:"iam_auth,omitempty"`
	SecretARN              string `json:"secret_arn,omitempty"`
	UserName               string `json:"user_name,omitempty"`
}

type targetGroup struct {
	Name                 string               `json:"name"`
	ARN                  string               `json:"arn"`
	ConnectionPoolConfig connectionPoolConfig `json:"connection_pool_config"`
}

type connectionPoolConfig struct {
	ConnectionBorrowTimeout   *int     `json:"connection_borrow_timeout,omitempty"`
	InitQuery                 *string  `json:"init_query,omitempty"`
	MaxConnectionsPercent     *int     `json:"max_connections_percent,omitempty"`
	MaxIdleConnectionsPercent *int     `json:"max_idle_connections_percent,omitempty"`
	SessionPinningFilters     []string `json:"session_pinning_filters,omitempty"`
}

type target struct {
	Identifier string `json:"identifier"`
	Type       string `json:"type"`
}

type tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type defaultEndpoint struct {
	Name                string `json:"name"`
	ARN                 string `json:"arn"`
	IsDefault           bool   `json:"is_default"`
	TargetRole          string `json:"target_role"`
	EndpointNetworkType string `json:"endpoint_network_type"`
}

// BuildBootstrap returns the plugin's version 1 one-shot bootstrap envelope.
// deploymentID and resourceID must stay stable for the life of the CR.
func BuildBootstrap(raw []byte, deploymentID, resourceID, region, assumeRoleARN, externalID, vpcID, endpoint string, credentials Credentials) ([]byte, error) {
	var source document
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("decode RDS document: %w", err)
	}
	if source.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported RDS document schema version %d", source.SchemaVersion)
	}
	if deploymentID == "" || resourceID == "" || region == "" || assumeRoleARN == "" || externalID == "" || vpcID == "" || endpoint == "" {
		return nil, fmt.Errorf("RDS bootstrap identity, region, role, external ID, VPC, and endpoint are required")
	}
	if source.DefaultEndpoint.Name == "" || source.Proxy.ARN == "" {
		return nil, fmt.Errorf("RDS document proxy identity is incomplete")
	}
	if source.Proxy.EngineFamily != engineFamilyMySQL {
		return nil, fmt.Errorf("unsupported RDS engine family %q", source.Proxy.EngineFamily)
	}
	if credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" {
		return nil, fmt.Errorf("internal RDS API credentials are required")
	}
	accountID, err := accountIDFromProxyARN(source.Proxy.ARN)
	if err != nil {
		return nil, err
	}
	targetGroupName := source.TargetGroup.Name
	if targetGroupName == "" {
		targetGroupName = defaultTargetGroupName
	}

	aws := map[string]any{
		"DBProxyName":                 source.DefaultEndpoint.Name,
		"DBProxyArn":                  source.Proxy.ARN,
		"EngineFamily":                source.Proxy.EngineFamily,
		"DefaultAuthScheme":           source.Proxy.DefaultAuthScheme,
		"RoleArn":                     assumeRoleARN,
		"ExternalId":                  externalID,
		"RequireTLS":                  source.Proxy.RequireTLS,
		"DebugLogging":                source.Proxy.DebugLogging,
		"IdleClientTimeout":           source.Proxy.IdleClientTimeout,
		"EndpointNetworkType":         source.Proxy.EndpointNetworkType,
		"TargetConnectionNetworkType": source.Proxy.TargetConnectionNetworkType,
		"Auth":                        authValues(source.Proxy.Auth),
		"TargetGroups": []any{map[string]any{
			"TargetGroupName":      targetGroupName,
			"WriterHostgroup":      WriterHostgroup,
			"ReaderHostgroup":      ReaderHostgroup,
			"ConnectionPoolConfig": poolValues(source.TargetGroup.ConnectionPoolConfig),
		}},
		"Targets":          targetValues(source.Targets),
		"Tags":             tagValues(source.Tags),
		"DBProxyEndpoints": endpointValues(source),
	}
	deleteEmptyStrings(aws, "DefaultAuthScheme", "EndpointNetworkType", "TargetConnectionNetworkType")

	configuration := map[string]any{
		"identity": map[string]any{
			"deployment_id": deploymentID,
			"resource_id":   resourceID,
			"resource_name": source.DefaultEndpoint.Name,
			"engine":        engineFamilyMySQL,
			"placement": map[string]any{
				"account_id":         accountID,
				"region":             region,
				"vpc_id":             vpcID,
				"subnet_ids":         source.Proxy.VpcSubnetIDs,
				"security_group_ids": source.Proxy.VpcSecurityGroupIDs,
				"endpoint":           endpoint,
			},
		},
		"scope": map[string]any{
			"hostgroups":  []int32{WriterHostgroup, ReaderHostgroup},
			"users":       []any{},
			"query_rules": []any{},
		},
		"tables":    emptyMySQLTables(),
		"variables": map[string]any{},
		"tls": map[string]any{
			"frontend": map[string]any{"require_tls": source.Proxy.RequireTLS},
		},
		"aws":           aws,
		"pool_policies": []any{},
	}

	secretDocument, err := json.Marshal(map[string]any{
		"secret_access_key": credentials.SecretAccessKey,
		"permissions": []string{
			"DescribeDBProxies", "ModifyDBProxy",
			"DescribeDBProxyTargetGroups", "ModifyDBProxyTargetGroup",
			"RegisterDBProxyTargets", "DeregisterDBProxyTargets", "DescribeDBProxyTargets",
			"DescribeDBProxyEndpoints", "ListTagsForResource",
			"AddTagsToResource", "RemoveTagsFromResource",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode internal RDS credential: %w", err)
	}
	manifest := map[string]any{
		"schema_version": 1,
		"configuration":  configuration,
		"credentials": []any{map[string]any{
			"access_key_id": credentials.AccessKeyID,
			"principal_id":  "proxysql-operator",
			"secret_ref":    "credential/proxysql-operator",
			"generation":    1,
			"revoked":       false,
		}},
		"secrets": map[string]string{
			"credential/proxysql-operator": string(secretDocument),
		},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode RDS bootstrap: %w", err)
	}
	return encoded, nil
}

func accountIDFromProxyARN(value string) (string, error) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "rds" || parts[4] == "" {
		return "", fmt.Errorf("RDS document proxy ARN is invalid")
	}
	accountID := parts[4]
	if len(accountID) != 12 {
		return "", fmt.Errorf("RDS document proxy ARN has an invalid AWS account ID")
	}
	for _, character := range accountID {
		if character < '0' || character > '9' {
			return "", fmt.Errorf("RDS document proxy ARN has an invalid AWS account ID")
		}
	}
	return accountID, nil
}

func authValues(values []auth) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		entry := map[string]any{
			"AuthScheme": value.AuthScheme, "ClientPasswordAuthType": value.ClientPasswordAuthType,
			"Description": value.Description, "IAMAuth": value.IAMAuth,
			"SecretArn": value.SecretARN, "UserName": value.UserName,
		}
		deleteEmptyStrings(entry, "AuthScheme", "ClientPasswordAuthType", "Description", "IAMAuth", "SecretArn", "UserName")
		result = append(result, entry)
	}
	return result
}

func poolValues(value connectionPoolConfig) map[string]any {
	result := map[string]any{}
	if value.ConnectionBorrowTimeout != nil {
		result["ConnectionBorrowTimeout"] = *value.ConnectionBorrowTimeout
	}
	if value.InitQuery != nil {
		result["InitQuery"] = *value.InitQuery
	}
	if value.MaxConnectionsPercent != nil {
		result["MaxConnectionsPercent"] = *value.MaxConnectionsPercent
	}
	if value.MaxIdleConnectionsPercent != nil {
		result["MaxIdleConnectionsPercent"] = *value.MaxIdleConnectionsPercent
	}
	if value.SessionPinningFilters != nil {
		result["SessionPinningFilters"] = value.SessionPinningFilters
	}
	return result
}

func targetValues(values []target) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]any{
			"Type": value.Type, "RdsResourceId": value.Identifier, "TargetGroupName": defaultTargetGroupName,
		})
	}
	return result
}

func tagValues(values []tag) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]any{"Key": value.Key, "Value": value.Value})
	}
	return result
}

func endpointValues(source document) []any {
	endpoint := map[string]any{
		"DBProxyEndpointName": source.DefaultEndpoint.Name,
		"DBProxyEndpointArn":  source.DefaultEndpoint.ARN,
		"DBProxyName":         source.DefaultEndpoint.Name,
		"IsDefault":           source.DefaultEndpoint.IsDefault,
		"TargetRole":          source.DefaultEndpoint.TargetRole,
		"EndpointNetworkType": source.DefaultEndpoint.EndpointNetworkType,
		"VpcSubnetIds":        source.Proxy.VpcSubnetIDs,
		"VpcSecurityGroupIds": source.Proxy.VpcSecurityGroupIDs,
	}
	deleteEmptyStrings(endpoint, "DBProxyEndpointArn", "TargetRole", "EndpointNetworkType")
	return []any{endpoint}
}

func emptyMySQLTables() map[string]any {
	return map[string]any{
		"mysql_servers":                      []any{},
		"mysql_users":                        []any{},
		"mysql_query_rules":                  []any{},
		"mysql_query_rules_fast_routing":     []any{},
		"mysql_hostgroup_attributes":         []any{},
		"mysql_replication_hostgroups":       []any{},
		"mysql_servers_ssl_params":           []any{},
		"mysql_group_replication_hostgroups": []any{},
		"mysql_galera_hostgroups":            []any{},
		"mysql_aws_aurora_hostgroups":        []any{},
	}
}

func deleteEmptyStrings(values map[string]any, keys ...string) {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && value == "" {
			delete(values, key)
		}
	}
}
