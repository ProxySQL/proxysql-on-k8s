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
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Service builds the load-balanced ClusterIP Service exposing MySQL, PostgreSQL,
// the admin port, and (optionally) the metrics and web UI ports. spec.service
// annotations and session affinity apply here only — never to the headless
// Service.
func (b *Builder) Service() *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        b.Name(),
			Namespace:   b.Namespace(),
			Labels:      b.Labels(),
			Annotations: b.Spec.Service.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: b.SelectorLabels(),
			Ports:    b.servicePorts(false),
		},
	}
	if t := b.Spec.Service.SessionAffinityTimeoutSeconds; t != nil {
		timeout := *t
		svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
		svc.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
			ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: &timeout},
		}
	}
	return svc
}

// HeadlessService builds the headless Service used as the StatefulSet's
// serviceName. publishNotReadyAddresses=true so the operator can reach pods
// during bootstrap (before they pass readiness).
func (b *Builder) HeadlessService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.HeadlessName(),
			Namespace: b.Namespace(),
			Labels:    b.Labels(),
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 b.SelectorLabels(),
			Ports:                    b.servicePorts(true),
		},
	}
}

// servicePorts returns the port list for either the regular or headless
// Service. Headless never exposes metrics or web.
func (b *Builder) servicePorts(headless bool) []corev1.ServicePort {
	var ports []corev1.ServicePort

	if b.Spec.Protocols.MySQL.IsEnabled() {
		ports = append(ports, corev1.ServicePort{
			Name:       "mysql",
			Port:       b.Spec.Protocols.MySQL.Port,
			TargetPort: intstr.FromString("mysql"),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	if b.Spec.Protocols.PostgreSQL.IsEnabled() {
		ports = append(ports, corev1.ServicePort{
			Name:       "pgsql",
			Port:       b.Spec.Protocols.PostgreSQL.Port,
			TargetPort: intstr.FromString("pgsql"),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	// Admin is always exposed (cluster-internal); the operator and ProxySQLConfig
	// reconciler need it.
	ports = append(ports, corev1.ServicePort{
		Name:       "admin",
		Port:       b.Spec.Protocols.Admin.Port,
		TargetPort: intstr.FromString("admin"),
		Protocol:   corev1.ProtocolTCP,
	})
	if !headless && isTrue(b.Spec.Metrics.Enabled) {
		ports = append(ports, corev1.ServicePort{
			Name:       "metrics",
			Port:       b.Spec.Metrics.Port,
			TargetPort: intstr.FromString("metrics"),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	if !headless && b.Spec.Protocols.Web.IsEnabled() {
		ports = append(ports, corev1.ServicePort{
			Name:       "web",
			Port:       b.Spec.Protocols.Web.Port,
			TargetPort: intstr.FromString("web"),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return ports
}
