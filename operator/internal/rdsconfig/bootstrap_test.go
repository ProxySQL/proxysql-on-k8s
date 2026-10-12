package rdsconfig

import (
	"encoding/json"
	"testing"
)

func TestBuildBootstrapAdaptsSaaSDocument(t *testing.T) {
	raw := []byte(`{
  "schema_version": 1,
  "proxy": {
    "arn": "arn:aws:rds:us-east-1:123456789012:db-proxy:proxy-1",
    "engine_family": "MYSQL",
    "auth": [{"auth_scheme":"SECRETS","iam_auth":"DISABLED","secret_arn":"arn:aws:secretsmanager:us-east-1:customer:secret:db"}],
    "default_auth_scheme": "NONE",
    "debug_logging": true,
    "idle_client_timeout": 1800,
    "require_tls": true,
    "endpoint_network_type": "IPV4",
    "target_connection_network_type": "IPV4",
    "vpc_subnet_ids": ["subnet-a"],
    "vpc_security_group_ids": ["sg-a"]
  },
  "target_group": {
    "name": "default",
    "arn": "arn:aws:rds:us-east-1:customer:target-group:default/proxy-1",
    "connection_pool_config": {"connection_borrow_timeout":120,"max_connections_percent":100,"max_idle_connections_percent":50}
  },
  "targets": [{"identifier":"orders","type":"TRACKED_CLUSTER"}],
  "tags": [{"key":"team","value":"payments"}],
  "default_endpoint": {"name":"orders-proxy","arn":"arn:aws:rds:us-east-1:customer:db-proxy-endpoint:proxy-1","is_default":true,"target_role":"READ_WRITE","endpoint_network_type":"IPV4","port":3306}
}`)

	encoded, err := BuildBootstrap(raw, "default/rds-orders", "cluster-uid", "us-east-1",
		"arn:aws:iam::123456789012:role/proxysql-rds", "vpc-customer", "rds-orders.default.svc",
		Credentials{AccessKeyID: "PXKEY", SecretAccessKey: "signing-secret"})
	if err != nil {
		t.Fatalf("BuildBootstrap: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	configuration := manifest["configuration"].(map[string]any)
	identity := configuration["identity"].(map[string]any)
	if identity["resource_name"] != "orders-proxy" || identity["resource_id"] != "cluster-uid" {
		t.Fatalf("identity = %+v", identity)
	}
	placement := identity["placement"].(map[string]any)
	if placement["account_id"] != "123456789012" || placement["vpc_id"] != "vpc-customer" ||
		placement["endpoint"] != "rds-orders.default.svc" {
		t.Fatalf("placement = %+v", placement)
	}
	aws := configuration["aws"].(map[string]any)
	if aws["DBProxyName"] != "orders-proxy" || aws["RoleArn"] != "arn:aws:iam::123456789012:role/proxysql-rds" {
		t.Fatalf("aws identity = %+v", aws)
	}
	groups := aws["TargetGroups"].([]any)
	group := groups[0].(map[string]any)
	if group["WriterHostgroup"] != float64(WriterHostgroup) || group["ReaderHostgroup"] != float64(ReaderHostgroup) {
		t.Fatalf("target group = %+v", group)
	}
	targets := aws["Targets"].([]any)
	if targets[0].(map[string]any)["RdsResourceId"] != "orders" {
		t.Fatalf("targets = %+v", targets)
	}
	credentials := manifest["credentials"].([]any)
	if credentials[0].(map[string]any)["access_key_id"] != "PXKEY" {
		t.Fatalf("credentials = %+v", credentials)
	}
}

func TestBuildBootstrapRejectsUnsupportedEngine(t *testing.T) {
	raw := []byte(`{"schema_version":1,"proxy":{"arn":"arn","engine_family":"POSTGRESQL"},"target_group":{"name":"default"},"default_endpoint":{"name":"proxy"}}`)
	if _, err := BuildBootstrap(raw, "deployment", "resource", "us-east-1", "arn:aws:iam::1:role/rds", "vpc-1", "proxy.svc",
		Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}); err == nil {
		t.Fatal("POSTGRESQL unexpectedly accepted in the MySQL S2 adapter")
	}
}
