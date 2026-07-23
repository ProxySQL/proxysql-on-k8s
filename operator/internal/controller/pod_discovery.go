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

// podEndpoint is a dialable ready replica: the pod-IP address the operator
// connects to, plus the pod's stable DNS identity for TLS verification.
type podEndpoint struct {
	// Addr is "<PodIP>:<port>". The operator dials IPs (they come straight
	// from the apiserver, no resolver in the path).
	Addr string
	// ServerName is "<pod>.<cluster>-headless.<ns>.svc" — the identity the
	// serving certificate covers (via the *.<cluster>-headless.<ns>.svc
	// wildcard SAN; the pod name is a single label). Pod IPs are NOT in the
	// SAN set — they churn on every pod recreation, so certificates can't
	// pin them. TLS dials therefore keep dialing the IP but set
	// tls.Config.ServerName to this DNS name, which crypto/tls verifies
	// against the certificate's SANs instead of the dialed address.
	ServerName string
}

// discoverPodEndpoints lists Pods owned by the ProxySQLCluster and returns
// an endpoint for any pod that has a Ready status and a non-empty IP,
// sorted by Addr (deterministic order for logs/status).
//
// Shared by both reconcilers (ProxySQLConfig pushes config to ready pods;
// ProxySQLCluster's restart-hash resolution and TLS rotation engine dial
// ready pods to apply changes without a rollout).
func discoverPodEndpoints(ctx context.Context, c client.Client, cluster *proxysqlv1alpha1.ProxySQLCluster, port int32) ([]podEndpoint, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		"proxysql.com/cluster": cluster.Name,
	}); err != nil {
		return nil, err
	}
	var out []podEndpoint
	for _, p := range pods.Items {
		if p.Status.PodIP == "" || p.DeletionTimestamp != nil {
			continue
		}
		if !isPodReady(&p) {
			continue
		}
		out = append(out, podEndpoint{
			Addr:       fmt.Sprintf("%s:%d", p.Status.PodIP, port),
			ServerName: podDNSName(p.Name, cluster.Name, cluster.Namespace),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out, nil
}

// podDNSName returns the stable per-pod DNS name behind the cluster's
// headless Service — the same name family the bootstrap cnf seeds into
// proxysql_servers and tlsutil.SANsFor covers with a wildcard SAN.
func podDNSName(podName, clusterName, namespace string) string {
	return fmt.Sprintf("%s.%s-headless.%s.svc", podName, clusterName, namespace)
}

func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
