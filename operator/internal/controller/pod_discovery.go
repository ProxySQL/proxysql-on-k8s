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

package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// discoverPodAddresses lists Pods owned by the ProxySQLCluster and returns
// host:port addresses for any pod that has a Ready status and a non-empty IP.
//
// Shared by both reconcilers (ProxySQLConfig pushes config to ready pods;
// ProxySQLCluster's restart-hash resolution dials ready pods to apply
// runtime-only variable changes without a rollout).
func discoverPodAddresses(ctx context.Context, c client.Client, cluster *proxysqlv1alpha1.ProxySQLCluster, port int32) ([]string, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		"proxysql.com/cluster": cluster.Name,
	}); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pods.Items {
		if p.Status.PodIP == "" || p.DeletionTimestamp != nil {
			continue
		}
		if !isPodReady(&p) {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d", p.Status.PodIP, port))
	}
	// Deterministic order for logs/status.
	sort.Strings(out)
	return out, nil
}

func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
