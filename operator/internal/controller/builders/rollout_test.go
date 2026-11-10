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
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// TestBuilder_NewFieldsUnset_ByteIdentical pins the upgrade-stability
// contract for updateStrategy, priorityClassName and the ServiceMonitor
// relabelings: leaving them unset (or empty) renders exactly what a spec
// without them renders. TestGolden covers the pod template; this also covers
// the StatefulSet spec outside the template and the ServiceMonitor.
func TestBuilder_NewFieldsUnset_ByteIdentical(t *testing.T) {
	smOn := func(c *proxysqlv1alpha1.ProxySQLCluster) { c.Spec.Metrics.ServiceMonitor.Enabled = true }
	plain := New(newCluster(clusterName, smOn), newScheme(t), Passwords{})
	empty := New(newCluster(clusterName, smOn, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.UpdateStrategy = nil
		c.Spec.PriorityClassName = ""
		c.Spec.Metrics.ServiceMonitor.Relabelings = []proxysqlv1alpha1.RelabelConfig{}
		c.Spec.Metrics.ServiceMonitor.MetricRelabelings = []proxysqlv1alpha1.RelabelConfig{}
	}), newScheme(t), Passwords{})

	if got, want := empty.StatefulSet("c"), plain.StatefulSet("c"); !reflect.DeepEqual(got, want) {
		t.Errorf("StatefulSet differs with the new fields unset/empty")
	}
	if got := plain.StatefulSet("c").Spec.UpdateStrategy; !reflect.DeepEqual(got, appsv1.StatefulSetUpdateStrategy{}) {
		t.Errorf("unset updateStrategy must leave the StatefulSet's strategy unset; got %+v", got)
	}
	if got, want := empty.ServiceMonitor(), plain.ServiceMonitor(); !reflect.DeepEqual(got, want) {
		t.Errorf("ServiceMonitor differs with empty relabelings:\n got %v\nwant %v", got.Object, want.Object)
	}
	ep := smEndpoint(t, plain)
	for _, k := range []string{"relabelings", "metricRelabelings"} {
		if _, ok := ep[k]; ok {
			t.Errorf("endpoint must omit %q when unset; got %v", k, ep)
		}
	}
}

func TestBuilder_StatefulSet_UpdateStrategyCopied(t *testing.T) {
	cases := map[string]appsv1.StatefulSetUpdateStrategy{
		"rolling partition": {
			Type: appsv1.RollingUpdateStatefulSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
				Partition:      int32Ptr(2),
				MaxUnavailable: new(intstr.FromInt32(1)),
			},
		},
		"on delete": {Type: appsv1.OnDeleteStatefulSetStrategyType},
	}
	for name, strategy := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
				c.Spec.UpdateStrategy = strategy.DeepCopy()
			})
			got := New(c, newScheme(t), Passwords{}).StatefulSet("c").Spec.UpdateStrategy
			if !reflect.DeepEqual(got, strategy) {
				t.Errorf("updateStrategy = %+v, want %+v", got, strategy)
			}
			// Builders are pure: the rendered object must not alias the spec.
			if got.RollingUpdate != nil && got.RollingUpdate == c.Spec.UpdateStrategy.RollingUpdate {
				t.Errorf("rendered rollingUpdate aliases the spec")
			}
		})
	}
}

func TestBuilder_StatefulSet_PriorityClassName(t *testing.T) {
	plain := New(newCluster(clusterName), newScheme(t), Passwords{}).StatefulSet("c")
	if got := plain.Spec.Template.Spec.PriorityClassName; got != "" {
		t.Errorf("unset priorityClassName rendered %q", got)
	}
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.PriorityClassName = "proxysql-critical"
	})
	ss := New(c, newScheme(t), Passwords{}).StatefulSet("c")
	if got := ss.Spec.Template.Spec.PriorityClassName; got != "proxysql-critical" {
		t.Errorf("priorityClassName = %q, want proxysql-critical", got)
	}
}

// TestPodSelector checks that the /scale selector is the StatefulSet's own
// selector, serialized, and that it matches the pods the StatefulSet makes.
func TestPodSelector(t *testing.T) {
	b := New(newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.PodLabels = map[string]string{"team": "db"}
	}), newScheme(t), Passwords{})
	ss := b.StatefulSet("c")

	got := PodSelector(clusterName)
	if want := metav1.FormatLabelSelector(ss.Spec.Selector); got != want {
		t.Errorf("PodSelector = %q, want the StatefulSet selector %q", got, want)
	}
	const want = "app.kubernetes.io/instance=pxc,app.kubernetes.io/name=proxysql,proxysql.com/cluster=pxc"
	if got != want {
		t.Errorf("PodSelector = %q, want %q", got, want)
	}
	sel, err := labels.Parse(got)
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	if !sel.Matches(labels.Set(ss.Spec.Template.Labels)) {
		t.Errorf("selector %q does not match pod labels %v", got, ss.Spec.Template.Labels)
	}
}

func TestBuilder_ServiceMonitor_Relabelings(t *testing.T) {
	sep, repl, empty := "/", "${1}", ""
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Metrics.ServiceMonitor.Enabled = true
		c.Spec.Metrics.ServiceMonitor.Relabelings = []proxysqlv1alpha1.RelabelConfig{
			{
				SourceLabels: []string{"__meta_kubernetes_pod_node_name", "namespace"},
				Separator:    &sep,
				TargetLabel:  "node",
				Regex:        "(.*)",
				Replacement:  &repl,
				Action:       "replace",
			},
			{SourceLabels: []string{"__address__"}, TargetLabel: "__tmp_hash", Modulus: 4, Action: "hashmod"},
		}
		c.Spec.Metrics.ServiceMonitor.MetricRelabelings = []proxysqlv1alpha1.RelabelConfig{
			{SourceLabels: []string{"__name__"}, Regex: "proxysql_debug_.*", Action: "drop"},
			// An explicit empty replacement is meaningful and must survive.
			{TargetLabel: "cluster", Replacement: &empty},
		}
	})
	b := New(c, newScheme(t), Passwords{})
	sm := b.ServiceMonitor()
	ep := smEndpoint(t, b)

	wantRelabel := []any{
		map[string]any{
			"sourceLabels": []any{"__meta_kubernetes_pod_node_name", "namespace"},
			"separator":    "/",
			"targetLabel":  "node",
			"regex":        "(.*)",
			"replacement":  "${1}",
			"action":       "replace",
		},
		map[string]any{
			"sourceLabels": []any{"__address__"},
			"targetLabel":  "__tmp_hash",
			"modulus":      int64(4),
			"action":       "hashmod",
		},
	}
	if !reflect.DeepEqual(ep["relabelings"], wantRelabel) {
		t.Errorf("relabelings =\n %#v\nwant\n %#v", ep["relabelings"], wantRelabel)
	}
	wantMetric := []any{
		map[string]any{
			"sourceLabels": []any{"__name__"},
			"regex":        "proxysql_debug_.*",
			"action":       "drop",
		},
		map[string]any{
			"targetLabel": "cluster",
			"replacement": "",
		},
	}
	if !reflect.DeepEqual(ep["metricRelabelings"], wantMetric) {
		t.Errorf("metricRelabelings =\n %#v\nwant\n %#v", ep["metricRelabelings"], wantMetric)
	}
	// The unstructured object must survive a deep copy (the reconciler and
	// the client both copy it); a non-JSON value type such as uint64 panics.
	_ = sm.DeepCopy()
}

func smEndpoint(t *testing.T, b *Builder) map[string]any {
	t.Helper()
	sm := b.ServiceMonitor()
	if sm == nil {
		t.Fatal("expected non-nil ServiceMonitor")
	}
	eps := sm.Object["spec"].(map[string]any)["endpoints"].([]any)
	if len(eps) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(eps))
	}
	return eps[0].(map[string]any)
}
