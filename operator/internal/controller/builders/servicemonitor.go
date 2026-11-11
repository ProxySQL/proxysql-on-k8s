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

package builders

import (
	"maps"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// ServiceMonitorGVK is the GroupVersionKind of the prometheus-operator
// monitoring.coreos.com/v1 ServiceMonitor resource.
var ServiceMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "ServiceMonitor",
}

// ServiceMonitor returns the desired ServiceMonitor for the cluster, or nil
// when either metrics or the ServiceMonitor sub-spec are disabled.
//
// Built as an unstructured object so the operator doesn't require the
// prometheus-operator Go types as a dependency. The cluster either has the
// monitoring.coreos.com CRD installed (and the CreateOrUpdate succeeds) or it
// doesn't (and the reconciler surfaces that as a non-fatal condition).
func (b *Builder) ServiceMonitor() *unstructured.Unstructured {
	if !isTrue(b.Spec.Metrics.Enabled) || !b.Spec.Metrics.ServiceMonitor.Enabled {
		return nil
	}

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(ServiceMonitorGVK)
	sm.SetName(b.Name())
	sm.SetNamespace(b.Namespace())

	labels := b.Labels()
	maps.Copy(labels, b.Spec.Metrics.ServiceMonitor.Labels)
	sm.SetLabels(labels)

	interval := b.Spec.Metrics.ServiceMonitor.Interval
	if interval == "" {
		interval = "30s"
	}
	scrapeTimeout := b.Spec.Metrics.ServiceMonitor.ScrapeTimeout
	if scrapeTimeout == "" {
		scrapeTimeout = "10s"
	}

	// selector.matchLabels must use the same string-map type Kubernetes wants
	// inside an unstructured object (map[string]interface{}). Convert.
	selectorLabels := map[string]any{}
	for k, v := range b.SelectorLabels() {
		selectorLabels[k] = v
	}

	endpoint := map[string]any{
		"port":          "metrics",
		"path":          "/metrics",
		"interval":      interval,
		"scrapeTimeout": scrapeTimeout,
	}

	// Omitted entirely when empty, so a spec without relabelings renders the
	// same ServiceMonitor as before the fields existed.
	if rc := relabelConfigs(b.Spec.Metrics.ServiceMonitor.Relabelings); rc != nil {
		endpoint["relabelings"] = rc
	}
	if rc := relabelConfigs(b.Spec.Metrics.ServiceMonitor.MetricRelabelings); rc != nil {
		endpoint["metricRelabelings"] = rc
	}

	sm.Object["spec"] = map[string]any{
		"selector": map[string]any{
			"matchLabels": selectorLabels,
		},
		"endpoints": []any{endpoint},
	}

	return sm
}

// relabelConfigs renders RelabelConfigs into the unstructured form of
// prometheus-operator's RelabelConfig, using its JSON field names. Unset
// fields are omitted; an explicitly empty separator/replacement (a non-nil
// pointer) is kept, since "" is meaningful there. Returns nil for an empty
// list. Values are restricted to the JSON-compatible types an unstructured
// object may hold (modulus is int64, not uint64).
func relabelConfigs(in []proxysqlv1alpha1.RelabelConfig) []any {
	if len(in) == 0 {
		return nil
	}
	out := make([]any, 0, len(in))
	for _, rc := range in {
		m := map[string]any{}
		if len(rc.SourceLabels) > 0 {
			src := make([]any, len(rc.SourceLabels))
			for i, l := range rc.SourceLabels {
				src[i] = l
			}
			m["sourceLabels"] = src
		}
		if rc.Separator != nil {
			m["separator"] = *rc.Separator
		}
		if rc.TargetLabel != "" {
			m["targetLabel"] = rc.TargetLabel
		}
		if rc.Regex != "" {
			m["regex"] = rc.Regex
		}
		if rc.Modulus != 0 {
			m["modulus"] = int64(rc.Modulus) // unstructured holds int64, never uint64
		}
		if rc.Replacement != nil {
			m["replacement"] = *rc.Replacement
		}
		if rc.Action != "" {
			m["action"] = rc.Action
		}
		out = append(out, m)
	}
	return out
}
