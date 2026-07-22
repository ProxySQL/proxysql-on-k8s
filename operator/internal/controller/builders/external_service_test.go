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

	corev1 "k8s.io/api/core/v1"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// withExternal returns a cluster mutator installing the given external
// Service spec (Enabled defaults to true unless the caller overrides it).
func withExternal(ext *proxysqlv1alpha1.ExternalServiceSpec) func(*proxysqlv1alpha1.ProxySQLCluster) {
	return func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Service.External = ext
	}
}

func externalPortNames(svc *corev1.Service) []string {
	names := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		names = append(names, p.Name)
	}
	return names
}

// Behavior 1: nil block or enabled=false → no external Service at all.
func TestBuilder_ExternalService_NilWhenAbsentOrDisabled(t *testing.T) {
	cases := []struct {
		name string
		ext  *proxysqlv1alpha1.ExternalServiceSpec
	}{
		{name: "block absent", ext: nil},
		{name: "explicitly disabled", ext: &proxysqlv1alpha1.ExternalServiceSpec{Enabled: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New(newCluster(clusterName, withExternal(tc.ext)), newScheme(t), Passwords{})
			if svc := b.ExternalService(); svc != nil {
				t.Errorf("ExternalService() = %+v, want nil", svc)
			}
		})
	}
}

// Behavior 2: enabled with empty Ports on a mysql+pgsql cluster → exactly the
// default data-plane set (mysql 6033, pgsql 6133), type LoadBalancer even
// though the Type zero value slipped past CRD defaulting.
func TestBuilder_ExternalService_DefaultPortSet(t *testing.T) {
	c := newCluster(clusterName,
		func(c *proxysqlv1alpha1.ProxySQLCluster) {
			c.Spec.Protocols.PostgreSQL.Enabled = boolPtr(true)
		},
		withExternal(&proxysqlv1alpha1.ExternalServiceSpec{Enabled: true}),
	)
	b := New(c, newScheme(t), Passwords{})
	svc := b.ExternalService()
	if svc == nil {
		t.Fatal("ExternalService() = nil, want a Service")
	}
	if svc.Name != clusterName+"-external" {
		t.Errorf("name = %q, want %q", svc.Name, clusterName+"-external")
	}
	if svc.Namespace != "default" {
		t.Errorf("namespace = %q, want default", svc.Namespace)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %q, want LoadBalancer (zero-value Type must default)", svc.Spec.Type)
	}
	want := []string{"mysql", "pgsql"}
	if got := externalPortNames(svc); !reflect.DeepEqual(got, want) {
		t.Fatalf("port names = %v, want %v", got, want)
	}
	if svc.Spec.Ports[0].Port != DefaultMySQLPort || svc.Spec.Ports[0].TargetPort.String() != "mysql" {
		t.Errorf("mysql port = %+v, want port %d targetPort mysql", svc.Spec.Ports[0], DefaultMySQLPort)
	}
	if svc.Spec.Ports[1].Port != DefaultPostgreSQLPort || svc.Spec.Ports[1].TargetPort.String() != "pgsql" {
		t.Errorf("pgsql port = %+v, want port %d targetPort pgsql", svc.Spec.Ports[1], DefaultPostgreSQLPort)
	}
}

// Behavior 3: a listener rides the external Service only when listed AND
// enabled — a pgsql entry under Ports is ignored while the protocol is
// disabled in the cluster spec.
func TestBuilder_ExternalService_ListedButDisabledProtocolFiltered(t *testing.T) {
	c := newCluster(clusterName, // pgsql stays at its disabled default
		withExternal(&proxysqlv1alpha1.ExternalServiceSpec{
			Enabled: true,
			Ports: map[string]proxysqlv1alpha1.ExternalPortSpec{
				"mysql": {},
				"pgsql": {},
			},
		}),
	)
	b := New(c, newScheme(t), Passwords{})
	svc := b.ExternalService()
	if svc == nil {
		t.Fatal("ExternalService() = nil, want a Service")
	}
	if got := externalPortNames(svc); !reflect.DeepEqual(got, []string{"mysql"}) {
		t.Errorf("port names = %v, want [mysql] (pgsql disabled in cluster spec)", got)
	}

	// Empty Ports on the same cluster also excludes the disabled protocol.
	b2 := New(newCluster(clusterName, withExternal(&proxysqlv1alpha1.ExternalServiceSpec{Enabled: true})), newScheme(t), Passwords{})
	if got := externalPortNames(b2.ExternalService()); !reflect.DeepEqual(got, []string{"mysql"}) {
		t.Errorf("default port names = %v, want [mysql]", got)
	}
}

// Behavior 4: a non-empty Ports map is an explicit selection — listeners not
// listed stay off even when their protocol is enabled.
func TestBuilder_ExternalService_ExplicitSelection(t *testing.T) {
	c := newCluster(clusterName,
		func(c *proxysqlv1alpha1.ProxySQLCluster) {
			c.Spec.Protocols.PostgreSQL.Enabled = boolPtr(true)
			c.Spec.Protocols.Web.Enabled = boolPtr(true)
		},
		withExternal(&proxysqlv1alpha1.ExternalServiceSpec{
			Enabled: true,
			Ports: map[string]proxysqlv1alpha1.ExternalPortSpec{
				"mysql": {},
				"web":   {},
			},
		}),
	)
	b := New(c, newScheme(t), Passwords{})
	svc := b.ExternalService()
	if svc == nil {
		t.Fatal("ExternalService() = nil, want a Service")
	}
	if got := externalPortNames(svc); !reflect.DeepEqual(got, []string{"mysql", "web"}) {
		t.Errorf("port names = %v, want [mysql web] (explicit selection, no pgsql)", got)
	}
}

// Behavior 5: admin rides the external Service ONLY behind ExposeAdmin. An
// "admin" key smuggled into Ports (CEL rejects it, but the builder must not
// trust admission alone) never exposes it.
func TestBuilder_ExternalService_AdminGate(t *testing.T) {
	t.Run("exposeAdmin true adds admin", func(t *testing.T) {
		c := newCluster(clusterName, withExternal(&proxysqlv1alpha1.ExternalServiceSpec{
			Enabled:     true,
			ExposeAdmin: true,
		}))
		b := New(c, newScheme(t), Passwords{})
		svc := b.ExternalService()
		found := false
		for _, p := range svc.Spec.Ports {
			if p.Name == "admin" {
				found = true
				if p.Port != DefaultAdminPort || p.TargetPort.String() != "admin" {
					t.Errorf("admin port = %+v, want port %d targetPort admin", p, DefaultAdminPort)
				}
			}
		}
		if !found {
			t.Errorf("exposeAdmin=true: admin port missing, have %v", externalPortNames(svc))
		}
	})

	t.Run("no exposeAdmin means no admin, ever", func(t *testing.T) {
		cases := map[string]map[string]proxysqlv1alpha1.ExternalPortSpec{
			"empty ports":              nil,
			"admin listed under Ports": {"admin": {}, "mysql": {}},
		}
		for name, ports := range cases {
			t.Run(name, func(t *testing.T) {
				c := newCluster(clusterName, withExternal(&proxysqlv1alpha1.ExternalServiceSpec{
					Enabled: true,
					Ports:   ports,
				}))
				b := New(c, newScheme(t), Passwords{})
				for _, p := range b.ExternalService().Spec.Ports {
					if p.Name == "admin" {
						t.Errorf("admin exposed without exposeAdmin (ports=%v)", ports)
					}
				}
			})
		}
	})
}

// Behavior 6: per-port nodePort pinning lands on the ServicePort.
func TestBuilder_ExternalService_NodePortPinning(t *testing.T) {
	c := newCluster(clusterName, withExternal(&proxysqlv1alpha1.ExternalServiceSpec{
		Enabled: true,
		Type:    corev1.ServiceTypeNodePort,
		Ports: map[string]proxysqlv1alpha1.ExternalPortSpec{
			"mysql": {NodePort: 30306},
		},
	}))
	b := New(c, newScheme(t), Passwords{})
	svc := b.ExternalService()
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("type = %q, want NodePort", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != "mysql" {
		t.Fatalf("ports = %v, want exactly [mysql]", externalPortNames(svc))
	}
	if got := svc.Spec.Ports[0].NodePort; got != 30306 {
		t.Errorf("nodePort = %d, want 30306", got)
	}
}

// Behavior 7: every tuning field passes through verbatim; nil
// AllocateLoadBalancerNodePorts becomes an explicit true pointer.
func TestBuilder_ExternalService_TuningPassthrough(t *testing.T) {
	lbClass := "service.k8s.aws/nlb"
	itp := corev1.ServiceInternalTrafficPolicyLocal
	ipPolicy := corev1.IPFamilyPolicyPreferDualStack
	c := newCluster(clusterName, withExternal(&proxysqlv1alpha1.ExternalServiceSpec{
		Enabled:                  true,
		Annotations:              map[string]string{"service.beta.kubernetes.io/aws-load-balancer-type": "nlb"},
		LoadBalancerClass:        &lbClass,
		ExternalTrafficPolicy:    corev1.ServiceExternalTrafficPolicyLocal,
		InternalTrafficPolicy:    &itp,
		LoadBalancerSourceRanges: []string{"10.0.0.0/8", "192.168.0.0/16"},
		HealthCheckNodePort:      31234,
		IPFamilyPolicy:           &ipPolicy,
		IPFamilies:               []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol},
	}))
	b := New(c, newScheme(t), Passwords{})
	svc := b.ExternalService()

	if got := svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-type"]; got != "nlb" {
		t.Errorf("annotation = %q, want nlb", got)
	}
	if svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != lbClass {
		t.Errorf("loadBalancerClass = %v, want %q", svc.Spec.LoadBalancerClass, lbClass)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("externalTrafficPolicy = %q, want Local", svc.Spec.ExternalTrafficPolicy)
	}
	if svc.Spec.InternalTrafficPolicy == nil || *svc.Spec.InternalTrafficPolicy != itp {
		t.Errorf("internalTrafficPolicy = %v, want Local", svc.Spec.InternalTrafficPolicy)
	}
	if !reflect.DeepEqual(svc.Spec.LoadBalancerSourceRanges, []string{"10.0.0.0/8", "192.168.0.0/16"}) {
		t.Errorf("loadBalancerSourceRanges = %v", svc.Spec.LoadBalancerSourceRanges)
	}
	if svc.Spec.AllocateLoadBalancerNodePorts == nil || !*svc.Spec.AllocateLoadBalancerNodePorts {
		t.Errorf("allocateLoadBalancerNodePorts = %v, want pointer-to-true when spec leaves it nil",
			svc.Spec.AllocateLoadBalancerNodePorts)
	}
	if svc.Spec.HealthCheckNodePort != 31234 {
		t.Errorf("healthCheckNodePort = %d, want 31234", svc.Spec.HealthCheckNodePort)
	}
	if svc.Spec.IPFamilyPolicy == nil || *svc.Spec.IPFamilyPolicy != ipPolicy {
		t.Errorf("ipFamilyPolicy = %v, want PreferDualStack", svc.Spec.IPFamilyPolicy)
	}
	if !reflect.DeepEqual(svc.Spec.IPFamilies, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}) {
		t.Errorf("ipFamilies = %v", svc.Spec.IPFamilies)
	}

	// Explicit false must survive (the whole reason the field is *bool).
	f := false
	c2 := newCluster(clusterName, withExternal(&proxysqlv1alpha1.ExternalServiceSpec{
		Enabled:                       true,
		AllocateLoadBalancerNodePorts: &f,
	}))
	svc2 := New(c2, newScheme(t), Passwords{}).ExternalService()
	if svc2.Spec.AllocateLoadBalancerNodePorts == nil || *svc2.Spec.AllocateLoadBalancerNodePorts {
		t.Errorf("explicit allocateLoadBalancerNodePorts=false lost: %v", svc2.Spec.AllocateLoadBalancerNodePorts)
	}
}

// Behavior 8: the external Service selects the same pods as the main Service.
func TestBuilder_ExternalService_SelectorMatchesMainService(t *testing.T) {
	b := New(newCluster(clusterName, withExternal(&proxysqlv1alpha1.ExternalServiceSpec{Enabled: true})), newScheme(t), Passwords{})
	svc := b.ExternalService()
	if !reflect.DeepEqual(svc.Spec.Selector, b.SelectorLabels()) {
		t.Errorf("selector = %v, want SelectorLabels() %v", svc.Spec.Selector, b.SelectorLabels())
	}
	if !reflect.DeepEqual(svc.Spec.Selector, b.Service().Spec.Selector) {
		t.Errorf("external selector %v diverges from main Service selector %v",
			svc.Spec.Selector, b.Service().Spec.Selector)
	}
	if svc.Labels["proxysql.com/cluster"] != clusterName {
		t.Errorf("labels missing cluster label: %v", svc.Labels)
	}
}
