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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ProxySQLClusterSpec defines the desired state of a ProxySQL control-plane
// cluster. The operator reconciles this into a StatefulSet, headless+regular
// Services, an admin Secret, and an optional PodDisruptionBudget.
type ProxySQLClusterSpec struct {
	// Replicas is the number of ProxySQL control-plane pods.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Pause stops the ProxySQL control-plane pods without deleting the
	// cluster: the StatefulSet is scaled to 0 while the Services, Secrets,
	// and PVCs are retained. Useful to cut compute cost during a
	// maintenance window or while backends are unavailable, without losing
	// the on-disk admin database or having to recreate the cluster. Set
	// back to false (or omit) to resume — the operator restores the
	// StatefulSet to spec.replicas, which is never modified by pausing.
	// Default false (per convention for default-off booleans; see
	// PersistenceSpec.Enabled for the default-true counterpart).
	// +optional
	Pause bool `json:"pause,omitempty"`

	// Image is the ProxySQL container image to run.
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// ImagePullSecrets is a list of secrets used to pull the image.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Auth references a Secret holding admin/radmin/monitor passwords.
	// When SecretName is empty, the operator creates a Secret with random
	// 32-char passwords (preserved across reconciles).
	// +optional
	Auth AuthSpec `json:"auth,omitempty"`

	// Persistence configures the per-pod PVC mounted at /var/lib/proxysql.
	// +optional
	Persistence PersistenceSpec `json:"persistence,omitempty"`

	// Protocols toggles which client-facing protocols are listening.
	// +optional
	Protocols ProtocolsSpec `json:"protocols,omitempty"`

	// Resources sets container resource requests and limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector for pod scheduling.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations for pod scheduling.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity for pod scheduling. No affinity is applied by default; set
	// explicitly to spread replicas across nodes/zones.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// PodSecurityContext defaults to PSA-restricted-compatible (non-root, fsGroup 999,
	// runtime/default seccomp). Override only if a specific image needs it.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// ContainerSecurityContext defaults to allowPrivilegeEscalation=false,
	// readOnlyRootFilesystem=true, capabilities drop=[ALL].
	// +optional
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`

	// Metrics controls the ProxySQL REST/Prometheus exporter port and optional ServiceMonitor.
	// +optional
	Metrics MetricsSpec `json:"metrics,omitempty"`

	// PodDisruptionBudget configures a PDB for the StatefulSet.
	// +optional
	PodDisruptionBudget PDBSpec `json:"podDisruptionBudget,omitempty"`

	// PodAnnotations and PodLabels are propagated to the pod template.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Service customizes the client-facing (regular) Service. The headless
	// Service used by the StatefulSet is not affected.
	// +optional
	Service ServiceSpec `json:"service,omitempty"`

	// Networking tunes pod-level networking (TCP keepalive sysctls).
	// +optional
	Networking NetworkingSpec `json:"networking,omitempty"`

	// Logging configures the optional Fluent Bit sidecar that ships
	// ProxySQL's query log (eventslog) to stdout, S3, or an HTTP collector.
	// Default off.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// Variables sets extra ProxySQL global variables baked into the
	// bootstrap cnf, in addition to the operator's own bootstrap-structural
	// settings (credentials, listening interfaces, etc).
	// +optional
	Variables VariablesSpec `json:"variables,omitempty"`

	// Probes overrides the proxysql container's startup/readiness/liveness
	// probes. Each field is a full corev1.Probe: when unset, the operator's
	// built-in default for that probe applies unchanged; when set, it
	// REPLACES the default probe entirely — there is no per-field merging
	// (e.g. setting readiness.periodSeconds alone still requires the rest of
	// the probe, such as the handler, to be specified). See ProbesSpec for
	// per-probe defaults and the readiness/backend-coupling warning.
	// +optional
	Probes ProbesSpec `json:"probes,omitempty"`

	// TLS configures certificate issuance and TLS wiring across the
	// frontend (mysql/pgsql client ports), admin/cluster-peering, and
	// backend (ProxySQL-to-database) surfaces. Absent (or Enabled=false)
	// renders exactly what the operator renders today — golden-pinned,
	// no upgrade restart. See TLSSpec for the three-tier issuance
	// precedence.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`
}

// TLSEnabled reports whether TLS is configured and turned on. Safe to call
// on a nil-TLS spec.
func (s *ProxySQLClusterSpec) TLSEnabled() bool { return s.TLS != nil && s.TLS.Enabled }

// ProbesSpec overrides the proxysql container's probes. Every field is a
// full corev1.Probe; a set field replaces the operator's default probe
// wholesale (handler, timings, thresholds — everything), not just the
// timing knobs. Leave a field unset to keep the operator's built-in default.
type ProbesSpec struct {
	// Startup overrides the container's startupProbe. No startup probe is
	// configured by default (nil), matching ProxySQL's fast, dependency-free
	// boot.
	// +optional
	Startup *corev1.Probe `json:"startup,omitempty"`

	// Readiness overrides the container's readinessProbe. Defaults to a TCP
	// check on the admin port (initialDelaySeconds=5, periodSeconds=5,
	// failureThreshold=3) — it only verifies ProxySQL's admin interface is
	// accepting connections, not that any backend is reachable.
	//
	// **Avoid backend-coupled readiness:** a custom readiness probe that
	// depends on a MySQL/PostgreSQL backend being reachable *through* the
	// proxy (e.g. an exec/HTTP probe that runs a query) ties every replica's
	// readiness to that backend's health. Because all replicas share the
	// same probe, a single backend outage can flip every ProxySQL pod to
	// NotReady at once, pulling the whole Service out of rotation — including
	// for traffic to backends that are perfectly healthy. Prefer probing
	// ProxySQL itself (the default) and let ProxySQL's own backend health
	// checks and query routing handle backend failures.
	// +optional
	Readiness *corev1.Probe `json:"readiness,omitempty"`

	// Liveness overrides the container's livenessProbe. Defaults to a TCP
	// check on the admin port (initialDelaySeconds=15, periodSeconds=10,
	// failureThreshold=3).
	// +optional
	Liveness *corev1.Probe `json:"liveness,omitempty"`
}

// VariablesSpec sets extra ProxySQL global variables in the bootstrap cnf.
// Keys are full variable names (admin-*, mysql-*, pgsql-*). Values render
// into the matching cnf section. Changes to runtime-settable variables are
// applied to running replicas via the admin interface without a restart;
// variables ProxySQL only honors at startup fall back to a rolling restart
// automatically (runtime read-back is the oracle).
type VariablesSpec struct {
	// Admin sets admin_variables. Keys must be prefixed "admin-".
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.startsWith('admin-'))",message="all keys must start with 'admin-'"
	Admin map[string]string `json:"admin,omitempty"`

	// MySQL sets mysql_variables. Keys must be prefixed "mysql-".
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.startsWith('mysql-'))",message="all keys must start with 'mysql-'"
	MySQL map[string]string `json:"mysql,omitempty"`

	// PostgreSQL sets pgsql_variables. Keys must be prefixed "pgsql-".
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.startsWith('pgsql-'))",message="all keys must start with 'pgsql-'"
	PostgreSQL map[string]string `json:"pgsql,omitempty"`
}

// LoggingSpec configures the optional Fluent Bit log-shipping sidecar.
// Per convention, default-off booleans stay plain bool (`*bool` is reserved
// for default-true fields).
//
// +kubebuilder:validation:XValidation:rule="!(has(self.enabled) && self.enabled) || (has(self.queryLog) && self.queryLog)",message="logging.queryLog is the only input; enable it or disable logging"
// +kubebuilder:validation:XValidation:rule="!(has(self.sinkType) && self.sinkType == 's3') || has(self.s3)",message="sinkType=s3 requires the s3 block"
// +kubebuilder:validation:XValidation:rule="!(has(self.sinkType) && self.sinkType == 'http') || has(self.http)",message="sinkType=http requires the http block"
type LoggingSpec struct {
	// Enabled adds the fluent-bit sidecar. Default off.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// QueryLog enables ProxySQL's eventslog (all MySQL-protocol queries) and
	// ships it. Currently the sidecar's only input, so admission rejects
	// enabled=true with queryLog=false until more inputs exist.
	//
	// LIMITATION: toggling queryLog OFF removes the eventslog lines from the
	// bootstrap cnf, and the container's --reload merge re-applies cnf lines
	// over proxysql.db but never deletes db entries absent from the cnf. On a
	// persistence-enabled cluster the saved eventslog settings therefore
	// survive the restart and the eventslog keeps running: run
	//   UPDATE global_variables SET variable_value='false'
	//     WHERE variable_name='mysql-eventslog_default_log';
	//   LOAD MYSQL VARIABLES TO RUNTIME; SAVE MYSQL VARIABLES TO DISK;
	// on the admin port (or set it via ProxySQLConfig.mysqlVariables).
	// +optional
	QueryLog bool `json:"queryLog,omitempty"`

	// SinkType selects where the log is shipped.
	// +optional
	// +kubebuilder:validation:Enum=stdout;s3;http
	// +kubebuilder:default=stdout
	SinkType string `json:"sinkType,omitempty"`

	// S3 configures the S3 sink. Required iff sinkType=s3.
	// +optional
	S3 *S3SinkSpec `json:"s3,omitempty"`

	// HTTP configures the HTTP sink. Required iff sinkType=http.
	// +optional
	HTTP *HTTPSinkSpec `json:"http,omitempty"`

	// Image is the Fluent Bit image. Pinned default; never `latest`.
	// +optional
	// +kubebuilder:default="fluent/fluent-bit:4.0.3"
	Image string `json:"image,omitempty"`

	// Resources for the sidecar. Defaults: requests 50m/64Mi, limits
	// 200m/128Mi.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// BufferSize bounds the logs emptyDir (sizeLimit) and the Fluent Bit
	// filesystem buffer. Default 1Gi.
	// +optional
	BufferSize resource.Quantity `json:"bufferSize,omitempty"`
}

// S3SinkSpec ships the query log to an S3 (or S3-compatible) bucket.
type S3SinkSpec struct {
	// Bucket is the destination bucket name.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Region is the AWS region of the bucket.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// Prefix is the object key prefix. Defaults to /proxysql/<cluster>.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Endpoint overrides the S3 endpoint for S3-compatible object stores.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// CredentialsSecretRef names a Secret with keys access-key-id /
	// secret-access-key. Credentials are NEVER inline in the CR; they reach
	// the sidecar as env vars.
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// HTTPSinkSpec ships the query log to a generic HTTP collector.
type HTTPSinkSpec struct {
	// Host is the collector hostname.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Port defaults to 443 when tls=true, else 80.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// URI is the request path. Defaults to "/".
	// +optional
	URI string `json:"uri,omitempty"`

	// TLS enables HTTPS towards the collector.
	// +optional
	TLS bool `json:"tls,omitempty"`

	// AuthTokenSecretRef names a Secret (key: token) sent as
	// `Authorization: Bearer <token>`. Optional; never inline — the token
	// reaches the sidecar as an env var.
	// +optional
	AuthTokenSecretRef *corev1.LocalObjectReference `json:"authTokenSecretRef,omitempty"`
}

// ServiceSpec customizes the client-facing (regular) Service.
type ServiceSpec struct {
	// Annotations are merged onto the Service (cloud LB configuration).
	// Spec keys win; annotations written by other controllers are preserved.
	// A key removed from this map lingers on the Service until removed by
	// hand (the operator cannot tell a removed spec key from a foreign one).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// SessionAffinityTimeoutSeconds enables ClientIP session affinity with
	// this timeout when set (1..86400).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	SessionAffinityTimeoutSeconds *int32 `json:"sessionAffinityTimeoutSeconds,omitempty"`

	// Type sets the client-facing Service's type. All enabled ports ride
	// this Service, including admin — for a curated external entry point
	// use External instead.
	// +optional
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	Type corev1.ServiceType `json:"type,omitempty"`

	// External creates a second, curated Service "<cluster>-external" for
	// out-of-cluster clients. Disabled (or absent) removes it.
	// +optional
	External *ExternalServiceSpec `json:"external,omitempty"`
}

// ExternalServiceSpec configures a second, curated Service
// "<cluster>-external" for out-of-cluster clients, independent of the main
// (internal) Service's type and annotations.
type ExternalServiceSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// +optional
	// +kubebuilder:default=LoadBalancer
	// +kubebuilder:validation:Enum=NodePort;LoadBalancer
	Type corev1.ServiceType `json:"type,omitempty"`

	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// +optional
	LoadBalancerClass *string `json:"loadBalancerClass,omitempty"`

	// +optional
	// +kubebuilder:validation:Enum=Cluster;Local
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`

	// InternalTrafficPolicy controls routing of traffic arriving via the
	// Service's cluster-internal IP (ClusterIP/node-local vs cluster-wide
	// endpoints). Independent of ExternalTrafficPolicy, which governs
	// traffic arriving via the external (NodePort/LoadBalancer) address.
	// +optional
	// +kubebuilder:validation:Enum=Cluster;Local
	InternalTrafficPolicy *corev1.ServiceInternalTrafficPolicy `json:"internalTrafficPolicy,omitempty"`

	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`

	// AllocateLoadBalancerNodePorts defaults to true; *bool so explicit
	// false survives marshalling (repo convention).
	// +optional
	// +kubebuilder:default=true
	AllocateLoadBalancerNodePorts *bool `json:"allocateLoadBalancerNodePorts,omitempty"`

	// HealthCheckNodePort is only meaningful with externalTrafficPolicy:
	// Local. 0 lets the API server allocate.
	// +optional
	// +kubebuilder:validation:Maximum=32767
	// +kubebuilder:validation:XValidation:rule="self == 0 || self >= 30000",message="healthCheckNodePort must be 0 (auto) or in 30000-32767"
	HealthCheckNodePort int32 `json:"healthCheckNodePort,omitempty"`

	// +optional
	// +kubebuilder:validation:Enum=SingleStack;PreferDualStack;RequireDualStack
	IPFamilyPolicy *corev1.IPFamilyPolicy `json:"ipFamilyPolicy,omitempty"`

	// +optional
	// +kubebuilder:validation:items:Enum=IPv4;IPv6
	IPFamilies []corev1.IPFamily `json:"ipFamilies,omitempty"`

	// Ports selects which listeners ride the external Service. Empty map =
	// default set: mysql + pgsql (each only if its protocol is enabled).
	// Valid keys: mysql, pgsql, web, metrics. Admin is deliberately NOT a
	// valid key — see ExposeAdmin.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['mysql','pgsql','web','metrics'])",message="valid port keys: mysql, pgsql, web, metrics"
	Ports map[string]ExternalPortSpec `json:"ports,omitempty"`

	// ExposeAdmin adds the admin port (6032). The ProxySQL admin interface
	// on a network edge is a serious risk; keep this false unless you have
	// source-range and NetworkPolicy controls in place.
	// +optional
	ExposeAdmin bool `json:"exposeAdmin,omitempty"`
}

// ExternalPortSpec tunes a single port riding the external Service.
type ExternalPortSpec struct {
	// NodePort pins the node port (30000-32767); 0 = auto-allocate.
	// +optional
	// +kubebuilder:validation:Maximum=32767
	// +kubebuilder:validation:XValidation:rule="self == 0 || self >= 30000",message="nodePort must be 0 (auto) or in 30000-32767"
	NodePort int32 `json:"nodePort,omitempty"`
}

// NetworkingSpec tunes pod-level networking behavior.
type NetworkingSpec struct {
	// TCPKeepalive sets the net.ipv4.tcp_keepalive_* sysctls on the pod.
	// These three sysctls are in the Kubernetes safe-sysctl set since v1.29
	// (KEP-3105) and are admitted under PSA `restricted`; on older clusters
	// the kubelet rejects them unless listed in --allowed-unsafe-sysctls.
	// +optional
	TCPKeepalive TCPKeepaliveSpec `json:"tcpKeepalive,omitempty"`
}

// TCPKeepaliveSpec maps to the net.ipv4.tcp_keepalive_{time,intvl,probes}
// kernel sysctls. Unset fields keep the node's kernel default. Bounds are
// conservative API-level limits; the kernel itself imposes no practical
// upper bound.
type TCPKeepaliveSpec struct {
	// Time is net.ipv4.tcp_keepalive_time: seconds a connection stays idle
	// before keepalive probes start.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32767
	Time *int32 `json:"time,omitempty"`

	// Interval is net.ipv4.tcp_keepalive_intvl: seconds between probes.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32767
	Interval *int32 `json:"interval,omitempty"`

	// Probes is net.ipv4.tcp_keepalive_probes: unanswered probes before the
	// connection is dropped.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=127
	Probes *int32 `json:"probes,omitempty"`
}

// ImageSpec selects a container image and pull policy.
type ImageSpec struct {
	// +optional
	// +kubebuilder:default=proxysql/proxysql
	Repository string `json:"repository,omitempty"`
	// +optional
	// +kubebuilder:default="3.0"
	Tag string `json:"tag,omitempty"`
	// +optional
	// +kubebuilder:default=IfNotPresent
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// AuthSpec references the Secret that holds admin/radmin/monitor passwords.
type AuthSpec struct {
	// SecretName is the name of an existing Secret. When empty, the operator
	// creates a Secret named after the ProxySQLCluster with random passwords.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Keys maps logical password names to keys inside the Secret.
	// Defaults match the operator-created Secret schema.
	// +optional
	Keys AuthKeys `json:"keys,omitempty"`
}

// AuthKeys names the Secret keys for each ProxySQL credential.
type AuthKeys struct {
	// +optional
	// +kubebuilder:default=admin-password
	AdminPassword string `json:"adminPassword,omitempty"`
	// +optional
	// +kubebuilder:default=radmin-password
	RadminPassword string `json:"radminPassword,omitempty"`
	// +optional
	// +kubebuilder:default=monitor-password
	MonitorPassword string `json:"monitorPassword,omitempty"`
}

// PersistenceSpec configures the per-pod PVC for /var/lib/proxysql.
type PersistenceSpec struct {
	// Enabled controls whether a PVC is mounted at /var/lib/proxysql. When
	// nil, the operator defaults to true; set false explicitly to disable
	// persistence and use an emptyDir instead.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +optional
	Size resource.Quantity `json:"size,omitempty"`
	// +optional
	StorageClass *string `json:"storageClass,omitempty"`
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// ProtocolsSpec toggles MySQL / PostgreSQL / admin listening.
type ProtocolsSpec struct {
	// +optional
	MySQL ProtocolSpec `json:"mysql,omitempty"`
	// +optional
	PostgreSQL ProtocolSpec `json:"pgsql,omitempty"`
	// +optional
	Admin ProtocolSpec `json:"admin,omitempty"`
	// Web exposes ProxySQL's built-in HTTPS stats web UI (admin web_enabled /
	// web_port). Disabled by default; a non-zero port implies enabled.
	// +optional
	Web ProtocolSpec `json:"web,omitempty"`
}

// ProtocolSpec configures one listening protocol.
type ProtocolSpec struct {
	// Enabled toggles this protocol's listener. When nil, the protocol's own
	// default applies: admin and mysql default to on, pgsql and web default
	// to off, and a non-zero Port implies enabled. An explicitly set value
	// always wins, even when Port is set — except admin, which is always on
	// (the operator needs it to push config) and ignores enabled=false.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// IsEnabled reports the resolved enabled state; nil counts as false.
// DefaultedSpec normalizes Enabled to non-nil, so post-defaulting reads are
// exact.
func (p ProtocolSpec) IsEnabled() bool { return p.Enabled != nil && *p.Enabled }

// MetricsSpec configures the ProxySQL Prometheus exporter (REST API).
type MetricsSpec struct {
	// Enabled exposes the ProxySQL REST/Prometheus endpoint. Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +optional
	// +kubebuilder:default=6070
	Port int32 `json:"port,omitempty"`
	// +optional
	ServiceMonitor ServiceMonitorSpec `json:"serviceMonitor,omitempty"`
}

// ServiceMonitorSpec configures the optional Prometheus Operator ServiceMonitor.
type ServiceMonitorSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +optional
	// +kubebuilder:default="30s"
	Interval string `json:"interval,omitempty"`
	// +optional
	// +kubebuilder:default="10s"
	ScrapeTimeout string `json:"scrapeTimeout,omitempty"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// PDBSpec configures a PodDisruptionBudget for the StatefulSet.
type PDBSpec struct {
	// Enabled toggles whether a PodDisruptionBudget is created. Defaults to
	// true; the PDB is still omitted when replicas <= 1.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +optional
	MinAvailable *intstr.IntOrString `json:"minAvailable,omitempty"`
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// TLSSpec configures certificate issuance and TLS wiring for the frontend
// (mysql/pgsql client ports), admin interface, cluster peering, and the
// separate backend (ProxySQL-to-database) surface.
//
// Tier resolution for the frontend/admin serving cert follows a strict
// precedence, evaluated in order: SecretName (tier 1, a user-provided
// kubernetes.io/tls Secret) wins whenever set; otherwise IssuerRef (tier 2,
// cert-manager) is used when its Name is set; otherwise the operator mints
// and manages a self-signed CA and serving cert (tier 3). Duration and
// RenewBefore apply only to tiers 2 and 3 — a user-supplied Secret (tier 1)
// is never re-issued by the operator.
type TLSSpec struct {
	// Enabled turns TLS on. Default off: absent or false renders exactly
	// what the operator renders today (golden-pinned; no upgrade restart).
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// SecretName is a user-provided kubernetes.io/tls Secret (tls.crt,
	// tls.key, optionally ca.crt) used as the frontend/admin serving
	// certificate. Tier 1: wins over IssuerRef and the operator's
	// self-signed fallback whenever set. The operator never rotates or
	// re-issues a Secret referenced this way — that's the caller's job.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// IssuerRef points at a cert-manager Issuer or ClusterIssuer used to
	// issue the frontend/admin serving certificate. Tier 2: used when
	// SecretName is empty and Name is set.
	// +optional
	IssuerRef *TLSIssuerRef `json:"issuerRef,omitempty"`

	// Duration is the issued certificate's lifetime. Applies to tiers 2
	// (cert-manager) and 3 (operator self-signed) only.
	// +optional
	// +kubebuilder:default="2160h"
	Duration metav1.Duration `json:"duration,omitempty"`

	// RenewBefore is how long before expiry the certificate is reissued.
	// Applies to tiers 2 (cert-manager) and 3 (operator self-signed) only.
	// +optional
	// +kubebuilder:default="720h"
	RenewBefore metav1.Duration `json:"renewBefore,omitempty"`

	// ExtraSANs adds DNS names or IPs to the issued serving certificate, on
	// top of the operator's default set (cluster name, headless/regular
	// Service DNS names, per-pod headless names). Useful for the external
	// Service's LoadBalancer hostname or a custom DNS record. Ignored for
	// tier 1 (SecretName) since that certificate is supplied as-is.
	// +optional
	ExtraSANs []string `json:"extraSANs,omitempty"`

	// Backend configures ProxySQL's TLS trust toward the backend databases
	// (mysql-ssl_p2s_ca / pgsql equivalent). This is a DIFFERENT PKI from
	// the frontend/admin serving certificate above: it must trust whatever
	// issued the *database's* server certificate, which is the operator's
	// own CA only when a single PKI happens to sign both sides. Absent
	// disables backend TLS variable rendering entirely.
	// +optional
	Backend *TLSBackendSpec `json:"backend,omitempty"`
}

// TLSIssuerRef references a cert-manager Issuer or ClusterIssuer used to
// issue TLS certificates (tier 2 of TLSSpec's resolution order).
type TLSIssuerRef struct {
	// Name of the Issuer or ClusterIssuer.
	Name string `json:"name"`

	// Kind of the referenced resource.
	// +optional
	// +kubebuilder:default=Issuer
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	Kind string `json:"kind,omitempty"`

	// Group of the referenced resource's API.
	// +optional
	// +kubebuilder:default=cert-manager.io
	Group string `json:"group,omitempty"`
}

// TLSBackendSpec configures ProxySQL's TLS trust and optional client
// certificate toward the backend databases. This is deliberately separate
// from the frontend/admin serving certificate configured by the rest of
// TLSSpec: ssl_p2s_ca must trust the *database's* issuer, which is a
// different PKI unless the same CA happens to sign both sides. Conflating
// the two (a naive single-secret model) would silently break backend
// certificate verification.
type TLSBackendSpec struct {
	// CASecretName references a Secret whose ca.crt trusts the backend
	// database's server certificate, rendered into mysql-ssl_p2s_ca (and
	// the pgsql equivalent). Unset: backend TLS variables are not
	// rendered at all.
	// +optional
	CASecretName string `json:"caSecretName,omitempty"`

	// ClientCertSecretName references an optional kubernetes.io/tls Secret
	// (tls.crt/tls.key) presented to the backend for mTLS, rendered into
	// ssl_p2s_cert / ssl_p2s_key.
	// +optional
	ClientCertSecretName string `json:"clientCertSecretName,omitempty"`
}

// ProxySQLClusterStatus reports observed state.
type ProxySQLClusterStatus struct {
	// ObservedGeneration is the most recent .metadata.generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the desired replica count.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the ready replica count of the underlying StatefulSet.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// UpdatedReplicas is the number of pods at the current StatefulSet
	// revision.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// Phase is a coarse, single-word projection of the conditions for
	// dashboards and external pollers. Conditions remain the source of truth.
	// One of: Pending, Creating, Running, Updating, Degraded, Failed,
	// Stopping, Paused. (Failed is reserved; the operator currently reports
	// Degraded for error states it can observe.) Stopping and Paused only
	// apply when spec.pause is true: Stopping while replicas are still
	// scaling down to 0, Paused once none are ready.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Endpoints are the in-cluster DNS endpoints (host:port) for every
	// enabled surface.
	// +optional
	Endpoints *ClusterEndpoints `json:"endpoints,omitempty"`

	// AdminSecretName is the Secret the operator wired in (created or referenced).
	// +optional
	AdminSecretName string `json:"adminSecretName,omitempty"`

	// Conditions follow standard K8s API conventions:
	//   Available, Progressing, Degraded
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterEndpoints lists in-cluster DNS endpoints (host:port) per surface,
// plus the out-of-cluster External entry point. A field is empty when that
// surface is disabled.
type ClusterEndpoints struct {
	// +optional
	MySQL string `json:"mysql,omitempty"`
	// +optional
	PostgreSQL string `json:"pgsql,omitempty"`
	// +optional
	Admin string `json:"admin,omitempty"`
	// +optional
	Web string `json:"web,omitempty"`
	// +optional
	Metrics string `json:"metrics,omitempty"`

	// External is the out-of-cluster entry point of the "<cluster>-external"
	// Service; empty unless spec.service.external is enabled. For type
	// LoadBalancer it is "host:port" — the first ingress IP (or hostname)
	// plus the Service's first port — and stays empty until the cloud
	// provider provisions the load balancer. For type NodePort it is the
	// comma-separated list of allocated node ports in the Service's port
	// order (host-less: every node IP serves them).
	// +optional
	External string `json:"external,omitempty"`
}

// Phase values for ProxySQLClusterStatus.Phase.
const (
	PhasePending  = "Pending"
	PhaseCreating = "Creating"
	PhaseRunning  = "Running"
	PhaseUpdating = "Updating"
	PhaseDegraded = "Degraded"
	PhaseFailed   = "Failed"
	// PhaseStopping applies only while spec.pause is true: the StatefulSet
	// is scaling down to 0 but at least one replica is still ready.
	PhaseStopping = "Stopping"
	// PhasePaused applies only while spec.pause is true and no replica is
	// ready: the StatefulSet is fully scaled to 0. Services, Secrets, and
	// PVCs are retained.
	PhasePaused = "Paused"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pxc
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.pause`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProxySQLCluster is the Schema for the proxysqlclusters API.
type ProxySQLCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ProxySQLCluster
	// +required
	Spec ProxySQLClusterSpec `json:"spec"`

	// status defines the observed state of ProxySQLCluster
	// +optional
	Status ProxySQLClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ProxySQLClusterList contains a list of ProxySQLCluster.
type ProxySQLClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ProxySQLCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProxySQLCluster{}, &ProxySQLClusterList{})
}
