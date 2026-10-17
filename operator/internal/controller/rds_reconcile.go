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

package controller

import (
	"context"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
	"github.com/ProxySQL/kubernetes/operator/internal/rdsclient"
	"github.com/ProxySQL/kubernetes/operator/internal/rdsconfig"
)

type rdsReconcileState struct {
	region      string
	credentials rdsclient.Credentials
	desired     rdsconfig.Desired
}

func (r *ProxySQLConfigReconciler) buildRDSReconcileState(ctx context.Context, cfg *proxysqlv1alpha1.ProxySQLConfig, cluster *proxysqlv1alpha1.ProxySQLCluster) (*rdsReconcileState, error) {
	if cfg.Spec.Plugin == nil || cfg.Spec.Plugin.AWS == nil || cfg.Spec.Plugin.AWS.DBProxy == nil {
		return nil, nil
	}
	if !cluster.Spec.AWSPluginEnabled() {
		return nil, fmt.Errorf("config has AWS DB proxy state but cluster has no AWS plugin bootstrap")
	}
	desired, err := rdsconfig.FromSpec(cfg.Spec.Plugin.AWS.DBProxy, cluster.Name)
	if err != nil {
		return nil, err
	}
	ref := cluster.Spec.Plugin.AWS.Bootstrap.ConfigMapRef
	var bootstrap corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: cluster.Namespace}, &bootstrap); err != nil {
		return nil, fmt.Errorf("read AWS bootstrap ConfigMap: %w", err)
	}
	region := bootstrap.Annotations[awsRegionAnnotation]
	if region == "" {
		return nil, fmt.Errorf("AWS bootstrap ConfigMap %q does not declare %s", ref.Name, awsRegionAnnotation)
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name + builders.AWSCredentialsSecretSuffix, Namespace: cluster.Namespace}, &secret); err != nil {
		return nil, fmt.Errorf("read RDS API credentials: %w", err)
	}
	credentials, err := credentialsFromSecret(&secret)
	if err != nil {
		return nil, fmt.Errorf("RDS API credential secret is incomplete: %w", err)
	}
	return &rdsReconcileState{region: region, credentials: credentials, desired: desired}, nil
}

func (state *rdsReconcileState) client(endpoint podEndpoint, dialTLS *adminTLS) (*rdsclient.Client, error) {
	host, _, err := net.SplitHostPort(endpoint.Addr)
	if err != nil {
		return nil, fmt.Errorf("parse replica address: %w", err)
	}
	httpClient, err := dialTLS.httpClient(endpoint.ServerName)
	if err != nil {
		return nil, err
	}
	return rdsclient.New("https://"+net.JoinHostPort(host, fmt.Sprint(builders.DefaultWebPort))+"/aws/rds", state.region, state.credentials, httpClient)
}

func (state *rdsReconcileState) desiredState() rdsclient.State {
	return rdsclient.State{Proxy: state.desired.Proxy, TargetGroup: state.desired.TargetGroup, Targets: state.desired.Targets, Tags: state.desired.Tags}
}

type replicaObservation struct {
	status  proxysqlv1alpha1.ProxySQLConfigReplicaStatus
	shunned int32
	drifted bool
}

func (r *ProxySQLConfigReconciler) observeReplica(ctx context.Context, endpoint podEndpoint, password string, native *proxysqlclient.Desired, rdsState *rdsReconcileState, dialTLS *adminTLS, version int64) replicaObservation {
	now := metav1.NewTime(time.Now())
	result := replicaObservation{status: proxysqlv1alpha1.ProxySQLConfigReplicaStatus{Pod: endpoint.Pod, LastCheckTime: &now}}
	pxc, err := dialTLS.dial(endpoint.Addr, password)
	if err != nil {
		result.status.Reason = "AdminUnavailable"
		result.drifted = true
		result.status.Drifted = true
		return result
	}
	runtimeState, err := proxysqlclient.ReadRuntime(ctx, pxc)
	_ = pxc.Close()
	if err != nil {
		result.status.Reason = "NativeReadFailed"
		result.drifted = true
		result.status.Drifted = true
		return result
	}
	result.shunned = runtimeState.ShunnedCount()
	if rdsState != nil {
		runtimeState = proxysqlclient.NativeRuntimeWithAWSDBProxy(runtimeState)
	}
	if diffs := native.Drift(runtimeState); len(diffs) != 0 {
		result.status.Reason = "NativeDrift"
		result.drifted = true
		result.status.Drifted = true
		return result
	}
	if rdsState != nil {
		client, err := rdsState.client(endpoint, dialTLS)
		if err != nil {
			result.status.Reason = "RDSClientUnavailable"
			result.drifted = true
			result.status.Drifted = true
			return result
		}
		observed, err := client.Describe(ctx, rdsState.desired.Proxy.Name)
		if err != nil {
			result.status.Reason = "RDSReadFailed"
			result.drifted = true
			result.status.Drifted = true
			return result
		}
		if diffs := rdsconfig.Drift(rdsState.desired, observed); len(diffs) != 0 {
			result.status.Reason = "RDSDrift"
			result.drifted = true
			result.status.Drifted = true
			return result
		}
	}
	result.status.Converged = true
	result.status.ObservedVersion = version
	result.status.Reason = "Converged"
	return result
}

func (r *ProxySQLConfigReconciler) applyReplica(ctx context.Context, endpoint podEndpoint, password string, native *proxysqlclient.Desired, rdsState *rdsReconcileState, dialTLS *adminTLS, version int64) replicaObservation {
	var current rdsclient.State
	steps := []applyCycleStep{}
	if rdsState != nil {
		steps = append(steps, applyCycleStep{"RDSClearFailed", func() error {
			client, err := rdsState.client(endpoint, dialTLS)
			if err != nil {
				return err
			}
			current, err = client.Describe(ctx, rdsState.desired.Proxy.Name)
			if err != nil {
				return err
			}
			return client.ClearMutable(ctx, current)
		}})
	}
	steps = append(steps, applyCycleStep{"NativeApplyFailed", func() error {
		pxc, err := dialTLS.dial(endpoint.Addr, password)
		if err != nil {
			return err
		}
		err = proxysqlclient.SyncWithAWSDBProxy(ctx, pxc, native, rdsState != nil)
		_ = pxc.Close()
		return err
	}})
	if rdsState != nil {
		steps = append(steps, applyCycleStep{"RDSApplyFailed", func() error {
			client, err := rdsState.client(endpoint, dialTLS)
			if err != nil {
				return err
			}
			current, err = client.Describe(ctx, rdsState.desired.Proxy.Name)
			if err != nil {
				return err
			}
			return client.Apply(ctx, current, rdsState.desiredState())
		}})
	}
	if reason := runApplyCycle(steps); reason != "" {
		return failedReplica(endpoint.Pod, reason)
	}
	return r.observeReplica(ctx, endpoint, password, native, rdsState, dialTLS, version)
}

type applyCycleStep struct {
	reason string
	run    func() error
}

func runApplyCycle(steps []applyCycleStep) string {
	for _, step := range steps {
		if err := step.run(); err != nil {
			return step.reason
		}
	}
	return ""
}

func failedReplica(pod, reason string) replicaObservation {
	now := metav1.NewTime(time.Now())
	return replicaObservation{status: proxysqlv1alpha1.ProxySQLConfigReplicaStatus{Pod: pod, Drifted: true, Reason: reason, LastCheckTime: &now}, drifted: true}
}

func summarizeReplicas(observations []replicaObservation) (statuses []proxysqlv1alpha1.ProxySQLConfigReplicaStatus, synced, drifted, shunned int32) {
	statuses = make([]proxysqlv1alpha1.ProxySQLConfigReplicaStatus, 0, len(observations))
	for _, observation := range observations {
		statuses = append(statuses, observation.status)
		shunned += observation.shunned
		if observation.status.Converged {
			synced++
		} else {
			drifted++
		}
	}
	return
}
