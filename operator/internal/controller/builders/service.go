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

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
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
			Type:     mainServiceType(b.Spec),
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

// mainServiceType returns the desired type for the main Service. The CRD
// defaults spec.service.type to ClusterIP, but unit tests can construct a
// spec with the zero value, so default explicitly rather than panic or emit
// an empty (invalid) Service.Spec.Type.
func mainServiceType(spec proxysqlv1alpha1.ProxySQLClusterSpec) corev1.ServiceType {
	if spec.Service.Type == "" {
		return corev1.ServiceTypeClusterIP
	}
	return spec.Service.Type
}

// Port names shared by every Service and the pod template's container
// ports. Constants so the goconst linter can hold the line on the literals.
const (
	portNameMySQL   = "mysql"
	portNamePgSQL   = "pgsql"
	portNameAdmin   = "admin"
	portNameWeb     = "web"
	portNameMetrics = "metrics"
)

// servicePort constructs a single named ServicePort. The single source of
// truth for the name/port/targetPort literals shared by the regular,
// headless, and external Services — a port change here changes all of them.
func servicePort(name string, port int32) corev1.ServicePort {
	return corev1.ServicePort{
		Name:       name,
		Port:       port,
		TargetPort: intstr.FromString(name),
		Protocol:   corev1.ProtocolTCP,
	}
}

// Per-listener ServicePort constructors, keyed off the defaulted spec.
func (b *Builder) mysqlServicePort() corev1.ServicePort {
	return servicePort(portNameMySQL, b.Spec.Protocols.MySQL.Port)
}

func (b *Builder) pgsqlServicePort() corev1.ServicePort {
	return servicePort(portNamePgSQL, b.Spec.Protocols.PostgreSQL.Port)
}

func (b *Builder) adminServicePort() corev1.ServicePort {
	return servicePort(portNameAdmin, b.Spec.Protocols.Admin.Port)
}

func (b *Builder) metricsServicePort() corev1.ServicePort {
	return servicePort(portNameMetrics, b.Spec.Metrics.Port)
}

func (b *Builder) webServicePort() corev1.ServicePort {
	return servicePort(portNameWeb, b.Spec.Protocols.Web.Port)
}

// servicePorts returns the port list for either the regular or headless
// Service. Headless never exposes metrics or web.
func (b *Builder) servicePorts(headless bool) []corev1.ServicePort {
	var ports []corev1.ServicePort

	if b.Spec.Protocols.MySQL.IsEnabled() {
		ports = append(ports, b.mysqlServicePort())
	}
	if b.Spec.Protocols.PostgreSQL.IsEnabled() {
		ports = append(ports, b.pgsqlServicePort())
	}
	// Admin is always exposed (cluster-internal); the operator and ProxySQLConfig
	// reconciler need it.
	ports = append(ports, b.adminServicePort())
	if !headless && isTrue(b.Spec.Metrics.Enabled) {
		ports = append(ports, b.metricsServicePort())
	}
	if !headless && b.Spec.Protocols.Web.IsEnabled() {
		ports = append(ports, b.webServicePort())
	}
	return ports
}
