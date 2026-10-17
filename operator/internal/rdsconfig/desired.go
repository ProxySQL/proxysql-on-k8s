/*
Copyright 2026 ProxySQL.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and limitations.
*/

package rdsconfig

import (
	"fmt"
	"reflect"
	"sort"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/rdsclient"
)

const defaultTargetGroupName = "default"

// Desired is the API-facing subset of the S1 document. Network placement and
// the default endpoint remain bootstrap/provisioning data; the existing-resource
// plugin operations reconcile the mutable proxy, pool, target, and tag state.
type Desired struct {
	Proxy       rdsclient.Proxy
	TargetGroup rdsclient.TargetGroup
	Targets     []rdsclient.Target
	Tags        []rdsclient.Tag
}

func FromSpec(source *proxysqlv1alpha1.AWSDBProxySpec, proxyName string) (Desired, error) {
	if source == nil || proxyName == "" {
		return Desired{}, fmt.Errorf("RDS proxy desired state is incomplete")
	}
	if source.EngineFamily != "MYSQL" {
		return Desired{}, fmt.Errorf("unsupported RDS engine family %q", source.EngineFamily)
	}
	if len(source.TargetGroups) != 1 || source.TargetGroups[0].TargetGroupName != defaultTargetGroupName {
		return Desired{}, fmt.Errorf("RDS proxy requires exactly one target group named %q", defaultTargetGroupName)
	}
	desired := Desired{
		Proxy: rdsclient.Proxy{
			Name: proxyName, EngineFamily: source.EngineFamily, DefaultAuthScheme: source.DefaultAuthScheme,
			DebugLogging: source.DebugLogging, IdleClientTimeout: int(source.IdleClientTimeout),
			RequireTLS: source.RequireTLS, RoleARN: source.RoleARN,
		},
		TargetGroup: rdsclient.TargetGroup{Name: source.TargetGroups[0].TargetGroupName, Pool: poolIntent(source.TargetGroups[0].ConnectionPoolConfig)},
	}
	for _, value := range source.Auth {
		desired.Proxy.Auth = append(desired.Proxy.Auth, rdsclient.Auth{
			AuthScheme: value.AuthScheme, ClientPasswordAuthType: value.ClientPasswordAuthType,
			Description: value.Description, IAMAuth: value.IAMAuth,
			SecretARN: value.SecretARN, UserName: value.UserName,
		})
	}
	for _, value := range source.Targets {
		if value.TargetGroupName != defaultTargetGroupName {
			return Desired{}, fmt.Errorf("target registration uses unsupported target group %q", value.TargetGroupName)
		}
		for _, identifier := range value.DBClusterIdentifiers {
			desired.Targets = append(desired.Targets, rdsclient.Target{Identifier: identifier, Type: "TRACKED_CLUSTER"})
		}
		for _, identifier := range value.DBInstanceIdentifiers {
			desired.Targets = append(desired.Targets, rdsclient.Target{Identifier: identifier, Type: "RDS_INSTANCE"})
		}
	}
	for _, value := range source.Tags {
		desired.Tags = append(desired.Tags, rdsclient.Tag{Key: value.Key, Value: value.Value})
	}
	normalizeDesired(&desired)
	return desired, nil
}

func poolIntent(value proxysqlv1alpha1.AWSConnectionPoolConfigSpec) rdsclient.Pool {
	pool := rdsclient.Pool{ConnectionBorrowTimeout: 120, MaxConnectionsPercent: 100, MaxIdleConnectionsPercent: 50}
	if value.ConnectionBorrowTimeout != nil {
		pool.ConnectionBorrowTimeout = int(*value.ConnectionBorrowTimeout)
	}
	if value.InitQuery != nil {
		pool.InitQuery = *value.InitQuery
	}
	if value.MaxConnectionsPercent != nil {
		pool.MaxConnectionsPercent = int(*value.MaxConnectionsPercent)
	}
	if value.MaxIdleConnectionsPercent != nil {
		pool.MaxIdleConnectionsPercent = int(*value.MaxIdleConnectionsPercent)
	}
	pool.SessionPinningFilters = append([]string(nil), value.SessionPinningFilters...)
	return pool
}

// Drift returns stable, credential-free domain names that differ. Detailed
// values are intentionally omitted so status and logs cannot expose auth data.
func Drift(desired Desired, observed rdsclient.State) []string {
	normalizeDesired(&desired)
	normalizeState(&observed)
	var drift []string
	if !reflect.DeepEqual(proxyIntent(desired.Proxy), proxyIntent(observed.Proxy)) {
		drift = append(drift, "proxy")
	}
	if !reflect.DeepEqual(desired.TargetGroup, observed.TargetGroup) {
		drift = append(drift, "target-group")
	}
	if !reflect.DeepEqual(desired.Targets, observed.Targets) {
		drift = append(drift, "targets")
	}
	if !reflect.DeepEqual(desired.Tags, observed.Tags) {
		drift = append(drift, "tags")
	}
	return drift
}

func normalizeDesired(value *Desired) {
	if value.TargetGroup.Name == "" {
		value.TargetGroup.Name = defaultTargetGroupName
	}
	sort.Strings(value.Proxy.VpcSecurityGroupIDs)
	sort.Slice(value.Proxy.Auth, func(i, j int) bool { return authKey(value.Proxy.Auth[i]) < authKey(value.Proxy.Auth[j]) })
	sort.Slice(value.Targets, func(i, j int) bool { return targetKey(value.Targets[i]) < targetKey(value.Targets[j]) })
	sort.Slice(value.Tags, func(i, j int) bool { return value.Tags[i].Key < value.Tags[j].Key })
	if value.Proxy.Auth == nil {
		value.Proxy.Auth = []rdsclient.Auth{}
	}
	if value.Proxy.VpcSecurityGroupIDs == nil {
		value.Proxy.VpcSecurityGroupIDs = []string{}
	}
	if value.TargetGroup.Pool.SessionPinningFilters == nil {
		value.TargetGroup.Pool.SessionPinningFilters = []string{}
	}
	if value.Targets == nil {
		value.Targets = []rdsclient.Target{}
	}
	if value.Tags == nil {
		value.Tags = []rdsclient.Tag{}
	}
}

func normalizeState(value *rdsclient.State) {
	desired := Desired{Proxy: value.Proxy, TargetGroup: value.TargetGroup, Targets: value.Targets, Tags: value.Tags}
	normalizeDesired(&desired)
	value.Proxy, value.TargetGroup, value.Targets, value.Tags = desired.Proxy, desired.TargetGroup, desired.Targets, desired.Tags
}

func proxyIntent(value rdsclient.Proxy) rdsclient.Proxy {
	value.ARN = ""
	value.EndpointNetworkType = ""
	value.TargetConnectionNetworkType = ""
	value.VpcSecurityGroupIDs = nil
	return value
}

func authKey(value rdsclient.Auth) string {
	return value.SecretARN + "\x00" + value.UserName + "\x00" + value.IAMAuth + "\x00" + value.AuthScheme
}

func targetKey(value rdsclient.Target) string { return value.Type + "\x00" + value.Identifier }
