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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// ExternalName returns the name of the curated external Service.
func (b *Builder) ExternalName() string { return b.Name() + "-external" }

// ExternalService builds the curated external Service "<cluster>-external"
// for out-of-cluster clients, or nil when spec.service.external is absent or
// disabled. Port policy: a listener rides the external Service only when it
// is selected (listed under Ports, or part of the default mysql+pgsql set
// when Ports is empty) AND its protocol is enabled in the cluster spec.
//
// Security invariant: the admin port is added only behind the ExposeAdmin
// boolean. "admin" is not a valid Ports key (CEL rejects it at admission),
// but the builder does not trust admission alone — an "admin" entry under
// Ports is ignored here regardless.
func (b *Builder) ExternalService() *corev1.Service {
	ext := b.Spec.Service.External
	if ext == nil || !ext.Enabled {
		return nil
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        b.ExternalName(),
			Namespace:   b.Namespace(),
			Labels:      b.Labels(),
			Annotations: ext.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:                  externalServiceType(ext),
			Selector:              b.SelectorLabels(),
			Ports:                 b.externalServicePorts(ext),
			ExternalTrafficPolicy: ext.ExternalTrafficPolicy,
			InternalTrafficPolicy: ext.InternalTrafficPolicy,
			IPFamilyPolicy:        ext.IPFamilyPolicy,
			IPFamilies:            ext.IPFamilies,
		},
	}
	// LoadBalancer-only fields. The apiserver rejects
	// allocateLoadBalancerNodePorts and loadBalancerClass on any other
	// Service type ("may only be used when 'type' is 'LoadBalancer'"), and
	// sourceRanges/healthCheckNodePort carry LB-only semantics — so on
	// NodePort these are dropped even when the CRD default populated them.
	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		svc.Spec.LoadBalancerClass = ext.LoadBalancerClass
		svc.Spec.LoadBalancerSourceRanges = ext.LoadBalancerSourceRanges
		svc.Spec.HealthCheckNodePort = ext.HealthCheckNodePort
		svc.Spec.AllocateLoadBalancerNodePorts = ext.AllocateLoadBalancerNodePorts
		// CRD default is true; apply it here too so a zero-value spec (unit
		// tests, older API servers) produces the same desired state.
		if svc.Spec.AllocateLoadBalancerNodePorts == nil {
			svc.Spec.AllocateLoadBalancerNodePorts = boolPtr(true)
		}
	}
	return svc
}

// externalServiceType defaults the zero value to LoadBalancer. The CRD
// defaults spec.service.external.type, but the builder must not rely on
// admission having run.
func externalServiceType(ext *proxysqlv1alpha1.ExternalServiceSpec) corev1.ServiceType {
	if ext.Type == "" {
		return corev1.ServiceTypeLoadBalancer
	}
	return ext.Type
}

// externalServicePorts derives the curated port list. Iteration is over a
// fixed listener order (never the Ports map) so the result is deterministic
// for the reconciler's diff.
func (b *Builder) externalServicePorts(ext *proxysqlv1alpha1.ExternalServiceSpec) []corev1.ServicePort {
	defaultSet := len(ext.Ports) == 0

	// selected reports whether a listener should ride the external Service:
	// listed under Ports (or in the default mysql+pgsql set) AND enabled.
	selected := func(key string, enabled, inDefaultSet bool) bool {
		if !enabled {
			return false
		}
		if defaultSet {
			return inDefaultSet
		}
		_, listed := ext.Ports[key]
		return listed
	}

	var ports []corev1.ServicePort
	add := func(key string, p corev1.ServicePort) {
		p.NodePort = ext.Ports[key].NodePort
		ports = append(ports, p)
	}

	if selected(portNameMySQL, b.Spec.Protocols.MySQL.IsEnabled(), true) {
		add(portNameMySQL, b.mysqlServicePort())
	}
	if selected(portNamePgSQL, b.Spec.Protocols.PostgreSQL.IsEnabled(), true) {
		add(portNamePgSQL, b.pgsqlServicePort())
	}
	if selected(portNameWeb, b.Spec.Protocols.Web.IsEnabled(), false) {
		add(portNameWeb, b.webServicePort())
	}
	if selected(portNameMetrics, isTrue(b.Spec.Metrics.Enabled), false) {
		add(portNameMetrics, b.metricsServicePort())
	}
	// Admin: gated exclusively by the ExposeAdmin boolean — a Ports entry is
	// deliberately never consulted (see the method comment).
	if ext.ExposeAdmin {
		ports = append(ports, b.adminServicePort())
	}
	return ports
}
