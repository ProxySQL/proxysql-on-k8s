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
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/ProxySQL/kubernetes/operator/internal/rdsclient"
)

// Desired is the API-facing subset of the S1 document. Network placement and
// the default endpoint remain bootstrap/provisioning data; the existing-resource
// plugin operations reconcile the mutable proxy, pool, target, and tag state.
type Desired struct {
	Proxy       rdsclient.Proxy
	TargetGroup rdsclient.TargetGroup
	Targets     []rdsclient.Target
	Tags        []rdsclient.Tag
}

func ParseDesired(raw []byte, assumeRoleARN string) (Desired, error) {
	var source document
	if err := json.Unmarshal(raw, &source); err != nil {
		return Desired{}, fmt.Errorf("decode RDS document: %w", err)
	}
	if source.SchemaVersion != 1 {
		return Desired{}, fmt.Errorf("unsupported RDS document schema version %d", source.SchemaVersion)
	}
	if source.Proxy.EngineFamily != engineFamilyMySQL || source.DefaultEndpoint.Name == "" || source.Proxy.ARN == "" {
		return Desired{}, fmt.Errorf("RDS document has an unsupported or incomplete proxy identity")
	}
	desired := Desired{
		Proxy: rdsclient.Proxy{
			Name: source.DefaultEndpoint.Name, ARN: source.Proxy.ARN,
			EngineFamily: source.Proxy.EngineFamily, DefaultAuthScheme: source.Proxy.DefaultAuthScheme,
			DebugLogging: source.Proxy.DebugLogging, IdleClientTimeout: source.Proxy.IdleClientTimeout,
			RequireTLS: source.Proxy.RequireTLS, RoleARN: assumeRoleARN,
			EndpointNetworkType:         source.Proxy.EndpointNetworkType,
			TargetConnectionNetworkType: source.Proxy.TargetConnectionNetworkType,
			VpcSecurityGroupIDs:         append([]string(nil), source.Proxy.VpcSecurityGroupIDs...),
		},
		TargetGroup: rdsclient.TargetGroup{Name: source.TargetGroup.Name, Pool: poolIntent(source.TargetGroup.ConnectionPoolConfig)},
	}
	for _, value := range source.Proxy.Auth {
		desired.Proxy.Auth = append(desired.Proxy.Auth, rdsclient.Auth{
			AuthScheme: value.AuthScheme, ClientPasswordAuthType: value.ClientPasswordAuthType,
			Description: value.Description, IAMAuth: value.IAMAuth,
			SecretARN: value.SecretARN, UserName: value.UserName,
		})
	}
	for _, value := range source.Targets {
		desired.Targets = append(desired.Targets, rdsclient.Target{Identifier: value.Identifier, Type: value.Type})
	}
	for _, value := range source.Tags {
		desired.Tags = append(desired.Tags, rdsclient.Tag{Key: value.Key, Value: value.Value})
	}
	normalizeDesired(&desired)
	return desired, nil
}

func poolIntent(value connectionPoolConfig) rdsclient.Pool {
	pool := rdsclient.Pool{ConnectionBorrowTimeout: 120, MaxConnectionsPercent: 100, MaxIdleConnectionsPercent: 50}
	if value.ConnectionBorrowTimeout != nil {
		pool.ConnectionBorrowTimeout = *value.ConnectionBorrowTimeout
	}
	if value.InitQuery != nil {
		pool.InitQuery = *value.InitQuery
	}
	if value.MaxConnectionsPercent != nil {
		pool.MaxConnectionsPercent = *value.MaxConnectionsPercent
	}
	if value.MaxIdleConnectionsPercent != nil {
		pool.MaxIdleConnectionsPercent = *value.MaxIdleConnectionsPercent
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
	if !reflect.DeepEqual(desired.Proxy, observed.Proxy) {
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

func authKey(value rdsclient.Auth) string {
	return value.SecretARN + "\x00" + value.UserName + "\x00" + value.IAMAuth + "\x00" + value.AuthScheme
}

func targetKey(value rdsclient.Target) string { return value.Type + "\x00" + value.Identifier }
