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

package rdsclient

import (
	"context"
	"fmt"
	"net/url"
)

const (
	paramDBProxyName     = "DBProxyName"
	paramTargetGroupName = "TargetGroupName"
	paramResourceName    = "ResourceName"
	defaultTargetGroup   = "default"
)

type Auth struct {
	AuthScheme             string `xml:"AuthScheme"`
	ClientPasswordAuthType string `xml:"ClientPasswordAuthType"`
	Description            string `xml:"Description"`
	IAMAuth                string `xml:"IAMAuth"`
	SecretARN              string `xml:"SecretArn"`
	UserName               string `xml:"UserName"`
}

type Proxy struct {
	Name                        string
	ARN                         string
	EngineFamily                string
	DefaultAuthScheme           string
	DebugLogging                bool
	IdleClientTimeout           int
	RequireTLS                  bool
	RoleARN                     string
	EndpointNetworkType         string
	TargetConnectionNetworkType string
	VpcSecurityGroupIDs         []string
	Auth                        []Auth
}

type Pool struct {
	ConnectionBorrowTimeout   int      `xml:"ConnectionBorrowTimeout"`
	InitQuery                 string   `xml:"InitQuery"`
	MaxConnectionsPercent     int      `xml:"MaxConnectionsPercent"`
	MaxIdleConnectionsPercent int      `xml:"MaxIdleConnectionsPercent"`
	SessionPinningFilters     []string `xml:"SessionPinningFilters>member"`
}

type TargetGroup struct {
	Name string
	Pool Pool
}

type Target struct {
	Identifier string `xml:"RdsResourceId"`
	Type       string `xml:"Type"`
}

type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type State struct {
	Proxy       Proxy
	TargetGroup TargetGroup
	Targets     []Target
	Tags        []Tag
}

type describeProxiesResponse struct {
	Items []struct {
		Name                        string   `xml:"DBProxyName"`
		ARN                         string   `xml:"DBProxyArn"`
		EngineFamily                string   `xml:"EngineFamily"`
		DefaultAuthScheme           string   `xml:"DefaultAuthScheme"`
		DebugLogging                bool     `xml:"DebugLogging"`
		IdleClientTimeout           int      `xml:"IdleClientTimeout"`
		RequireTLS                  bool     `xml:"RequireTLS"`
		RoleARN                     string   `xml:"RoleArn"`
		EndpointNetworkType         string   `xml:"EndpointNetworkType"`
		TargetConnectionNetworkType string   `xml:"TargetConnectionNetworkType"`
		VpcSecurityGroupIDs         []string `xml:"VpcSecurityGroupIds>member"`
		Auth                        []Auth   `xml:"Auth>member"`
	} `xml:"DescribeDBProxiesResult>DBProxies>member"`
}

type describeGroupsResponse struct {
	Items []struct {
		Name string `xml:"TargetGroupName"`
		Pool Pool   `xml:"ConnectionPoolConfig"`
	} `xml:"DescribeDBProxyTargetGroupsResult>TargetGroups>member"`
}

type describeTargetsResponse struct {
	Items []Target `xml:"DescribeDBProxyTargetsResult>Targets>member"`
}

type listTagsResponse struct {
	Items []Tag `xml:"ListTagsForResourceResult>TagList>Tag"`
}

// Describe reads the complete RDS state used by the S2 convergence decision.
func (c *Client) Describe(ctx context.Context, proxyARN string) (State, error) {
	var state State
	var proxies describeProxiesResponse
	if err := c.Call(ctx, "DescribeDBProxies", nil, &proxies); err != nil {
		return state, err
	}
	if len(proxies.Items) != 1 {
		return state, fmt.Errorf("DescribeDBProxies returned %d proxies", len(proxies.Items))
	}
	p := proxies.Items[0]
	state.Proxy = Proxy{Name: p.Name, ARN: p.ARN, EngineFamily: p.EngineFamily,
		DefaultAuthScheme: p.DefaultAuthScheme, DebugLogging: p.DebugLogging,
		IdleClientTimeout: p.IdleClientTimeout, RequireTLS: p.RequireTLS,
		RoleARN: p.RoleARN, EndpointNetworkType: p.EndpointNetworkType,
		TargetConnectionNetworkType: p.TargetConnectionNetworkType,
		VpcSecurityGroupIDs:         p.VpcSecurityGroupIDs, Auth: p.Auth}

	var groups describeGroupsResponse
	if err := c.Call(ctx, "DescribeDBProxyTargetGroups", url.Values{
		paramDBProxyName: {state.Proxy.Name}, paramTargetGroupName: {defaultTargetGroup},
	}, &groups); err != nil {
		return state, err
	}
	if len(groups.Items) != 1 {
		return state, fmt.Errorf("DescribeDBProxyTargetGroups returned %d groups", len(groups.Items))
	}
	state.TargetGroup = TargetGroup{Name: groups.Items[0].Name, Pool: groups.Items[0].Pool}

	var targets describeTargetsResponse
	if err := c.Call(ctx, "DescribeDBProxyTargets", url.Values{
		paramDBProxyName: {state.Proxy.Name}, paramTargetGroupName: {defaultTargetGroup},
	}, &targets); err != nil {
		return state, err
	}
	state.Targets = targets.Items

	var tags listTagsResponse
	if err := c.Call(ctx, "ListTagsForResource", url.Values{paramResourceName: {proxyARN}}, &tags); err != nil {
		return state, err
	}
	state.Tags = tags.Items
	return state, nil
}

func (c *Client) ModifyProxy(ctx context.Context, currentName string, proxy Proxy) error {
	values := url.Values{paramDBProxyName: {currentName}, "Auth": {""}}
	if currentName != proxy.Name {
		values.Set("NewDBProxyName", proxy.Name)
	}
	if proxy.DefaultAuthScheme != "" {
		values.Set("DefaultAuthScheme", proxy.DefaultAuthScheme)
	}
	setBool(values, "DebugLogging", proxy.DebugLogging)
	setInt(values, "IdleClientTimeout", proxy.IdleClientTimeout)
	setBool(values, "RequireTLS", proxy.RequireTLS)
	if proxy.RoleARN != "" {
		values.Set("RoleArn", proxy.RoleARN)
	}
	for i, group := range proxy.VpcSecurityGroupIDs {
		values.Set(fmt.Sprintf("SecurityGroups.member.%d", i+1), group)
	}
	for i, auth := range proxy.Auth {
		prefix := fmt.Sprintf("Auth.member.%d.", i+1)
		for name, value := range map[string]string{"AuthScheme": auth.AuthScheme, "ClientPasswordAuthType": auth.ClientPasswordAuthType, "Description": auth.Description, "IAMAuth": auth.IAMAuth, "SecretArn": auth.SecretARN, "UserName": auth.UserName} {
			if value != "" {
				values.Set(prefix+name, value)
			}
		}
	}
	if len(proxy.Auth) > 0 {
		values.Del("Auth")
	}
	return c.Call(ctx, "ModifyDBProxy", values, nil)
}

// ClearMutable removes RDS intent that can project plugin-owned native rows.
// The bootstrapped proxy and default target-group identities remain installed.
func (c *Client) ClearMutable(ctx context.Context, state State) error {
	if err := c.DeregisterTargets(ctx, state.Proxy.Name, state.Targets); err != nil {
		return err
	}
	if err := c.RemoveTags(ctx, state.Proxy.ARN, state.Tags); err != nil {
		return err
	}
	if err := c.Call(ctx, "ModifyDBProxy", url.Values{paramDBProxyName: {state.Proxy.Name}, "Auth": {""}}, nil); err != nil {
		return err
	}
	return c.ModifyPool(ctx, state.Proxy.Name, Pool{ConnectionBorrowTimeout: 120, MaxConnectionsPercent: 100, MaxIdleConnectionsPercent: 50})
}

func (c *Client) Apply(ctx context.Context, current State, desired State) error {
	if err := c.ModifyProxy(ctx, current.Proxy.Name, desired.Proxy); err != nil {
		return err
	}
	if err := c.ModifyPool(ctx, desired.Proxy.Name, desired.TargetGroup.Pool); err != nil {
		return err
	}
	if err := c.RegisterTargets(ctx, desired.Proxy.Name, desired.Targets); err != nil {
		return err
	}
	return c.AddTags(ctx, desired.Proxy.ARN, desired.Tags)
}

func (c *Client) ModifyPool(ctx context.Context, proxyName string, pool Pool) error {
	values := url.Values{paramDBProxyName: {proxyName}, paramTargetGroupName: {defaultTargetGroup}, "ConnectionPoolConfig.SessionPinningFilters": {""}}
	setInt(values, "ConnectionPoolConfig.ConnectionBorrowTimeout", pool.ConnectionBorrowTimeout)
	setInt(values, "ConnectionPoolConfig.MaxConnectionsPercent", pool.MaxConnectionsPercent)
	setInt(values, "ConnectionPoolConfig.MaxIdleConnectionsPercent", pool.MaxIdleConnectionsPercent)
	values.Set("ConnectionPoolConfig.InitQuery", pool.InitQuery)
	for i, value := range pool.SessionPinningFilters {
		values.Set(fmt.Sprintf("ConnectionPoolConfig.SessionPinningFilters.member.%d", i+1), value)
	}
	if len(pool.SessionPinningFilters) > 0 {
		values.Del("ConnectionPoolConfig.SessionPinningFilters")
	}
	return c.Call(ctx, "ModifyDBProxyTargetGroup", values, nil)
}

func (c *Client) RegisterTargets(ctx context.Context, proxyName string, targets []Target) error {
	if len(targets) == 0 {
		return nil
	}
	values := targetValues(proxyName, targets)
	return c.Call(ctx, "RegisterDBProxyTargets", values, nil)
}

func (c *Client) DeregisterTargets(ctx context.Context, proxyName string, targets []Target) error {
	if len(targets) == 0 {
		return nil
	}
	return c.Call(ctx, "DeregisterDBProxyTargets", targetValues(proxyName, targets), nil)
}

func targetValues(proxyName string, targets []Target) url.Values {
	values := url.Values{paramDBProxyName: {proxyName}, paramTargetGroupName: {defaultTargetGroup}}
	instances, clusters := 0, 0
	for _, target := range targets {
		if target.Type == "RDS_INSTANCE" {
			instances++
			values.Set(fmt.Sprintf("DBInstanceIdentifiers.member.%d", instances), target.Identifier)
		} else {
			clusters++
			values.Set(fmt.Sprintf("DBClusterIdentifiers.member.%d", clusters), target.Identifier)
		}
	}
	return values
}

func (c *Client) AddTags(ctx context.Context, proxyARN string, tags []Tag) error {
	if len(tags) == 0 {
		return nil
	}
	values := url.Values{paramResourceName: {proxyARN}}
	for i, tag := range tags {
		values.Set(fmt.Sprintf("Tags.Tag.%d.Key", i+1), tag.Key)
		values.Set(fmt.Sprintf("Tags.Tag.%d.Value", i+1), tag.Value)
	}
	return c.Call(ctx, "AddTagsToResource", values, nil)
}

func (c *Client) RemoveTags(ctx context.Context, proxyARN string, tags []Tag) error {
	if len(tags) == 0 {
		return nil
	}
	values := url.Values{paramResourceName: {proxyARN}}
	for i, tag := range tags {
		values.Set(fmt.Sprintf("TagKeys.member.%d", i+1), tag.Key)
	}
	return c.Call(ctx, "RemoveTagsFromResource", values, nil)
}
