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

// Package builders constructs the Kubernetes resources owned by a
// ProxySQLCluster. Each builder method returns a desired-state object; the
// reconciler diffs it against the cluster state and applies updates.
package builders

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// Default ports if the spec doesn't override.
const (
	DefaultAdminPort       int32 = 6032
	DefaultMySQLPort       int32 = 6033
	DefaultPostgreSQLPort  int32 = 6133
	DefaultMetricsPort     int32 = 6070
	DefaultWebPort         int32 = 6080
	DefaultProxySQLImage         = "proxysql/proxysql"
	DefaultProxySQLTag           = "3.0"
	DefaultPersistenceSize       = "1Gi"
)

// Default ProxySQL secret key names. Match the AuthKeys defaults on the CRD.
const (
	SecretKeyAdminPassword   = "admin-password"
	SecretKeyRadminPassword  = "radmin-password"
	SecretKeyMonitorPassword = "monitor-password"
)

// Passwords holds the plaintext credentials the operator renders into the
// bootstrap ConfigMap. ExtraAdminUser/ExtraAdminPassword carry an additional
// remote-capable admin credential derived from a username/password-shaped
// external Secret (empty when unused).
type Passwords struct {
	Admin   string
	Radmin  string
	Monitor string

	ExtraAdminUser     string
	ExtraAdminPassword string
}

// cnfUsernameRe constrains usernames rendered into the bootstrap cnf's
// admin_credentials value (a double-quoted string of ';'-separated
// "user:pass" pairs). Only conservative identifier characters are allowed.
var cnfUsernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// validCnfPassword rejects passwords that cannot be represented safely in
// the bootstrap proxysql.cnf. Cnf grammar: values live inside double-quoted
// strings, and admin_credentials is additionally split on ';' — so a '"',
// ';', any control character (< 0x20), or DEL would corrupt the rendered
// config or break ProxySQL's credential splitting. Such values could never
// work anyway, so rejecting them loses nothing.
func validCnfPassword(s string) error {
	for _, r := range s {
		switch {
		case r == '"' || r == ';':
			return fmt.Errorf("contains forbidden character %q", r)
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("contains a control character (%#x)", r)
		}
	}
	return nil
}

// PasswordsFromSecret resolves credentials from an auth Secret's data,
// accepting two schemas in precedence order:
//  1. the operator schema (keys per AuthKeys) — all three keys required;
//  2. the common platform schema: "username"/"password" (+ optional
//     monitor key). Admin and radmin share the password; a username other
//     than admin/radmin becomes an extra admin credential in the cnf.
//
// A partial operator schema (the admin or radmin key present without the
// other two) errors to prevent silent credential substitution: falling
// through to the username/password schema would discard explicitly
// configured keys. The monitor key alone is not "partial" — it's the
// documented optional override for schema 2.
//
// All returned credentials are validated against the cnf grammar (values
// live inside double-quoted strings split on ';'); see validCnfPassword.
func PasswordsFromSecret(data map[string][]byte, keys proxysqlv1alpha1.AuthKeys) (Passwords, error) {
	admin := string(data[keys.AdminPassword])
	radmin := string(data[keys.RadminPassword])
	monitor := string(data[keys.MonitorPassword])

	if admin != "" || radmin != "" {
		var missing []string
		if admin == "" {
			missing = append(missing, keys.AdminPassword)
		}
		if radmin == "" {
			missing = append(missing, keys.RadminPassword)
		}
		if monitor == "" {
			missing = append(missing, keys.MonitorPassword)
		}
		if len(missing) > 0 {
			return Passwords{}, fmt.Errorf(
				"auth secret has a partial operator schema: missing key(s) %s",
				strings.Join(missing, ", "))
		}
		for key, val := range map[string]string{
			keys.AdminPassword:   admin,
			keys.RadminPassword:  radmin,
			keys.MonitorPassword: monitor,
		} {
			if err := validCnfPassword(val); err != nil {
				return Passwords{}, fmt.Errorf("auth secret key %q: %w", key, err)
			}
		}
		return Passwords{Admin: admin, Radmin: radmin, Monitor: monitor}, nil
	}

	user := string(data["username"])
	pass := string(data["password"])
	if user != "" && pass != "" {
		if !cnfUsernameRe.MatchString(user) {
			return Passwords{}, fmt.Errorf(
				"auth secret key \"username\" must match %s", cnfUsernameRe.String())
		}
		if err := validCnfPassword(pass); err != nil {
			return Passwords{}, fmt.Errorf("auth secret key \"password\": %w", err)
		}
		pw := Passwords{Admin: pass, Radmin: pass, Monitor: pass}
		if monitor != "" {
			if err := validCnfPassword(monitor); err != nil {
				return Passwords{}, fmt.Errorf("auth secret key %q: %w", keys.MonitorPassword, err)
			}
			pw.Monitor = monitor
		}
		if user != "admin" && user != "radmin" {
			pw.ExtraAdminUser = user
			pw.ExtraAdminPassword = pass
		}
		return pw, nil
	}

	return Passwords{}, fmt.Errorf(
		"auth secret matches neither schema: need %s/%s/%s, or username/password",
		keys.AdminPassword, keys.RadminPassword, keys.MonitorPassword)
}

// Builder constructs the K8s objects owned by a ProxySQLCluster.
//
// Construct one per reconcile call. The Builder holds resolved values
// (defaulted spec, passwords) so individual builder methods stay pure.
type Builder struct {
	Cluster *proxysqlv1alpha1.ProxySQLCluster
	Scheme  *runtime.Scheme
	Spec    proxysqlv1alpha1.ProxySQLClusterSpec // already defaulted
	Pw      Passwords
}

// New returns a Builder with .Spec already defaulted. Pass the resolved
// admin/radmin/monitor passwords (read from the operator-managed Secret).
func New(cluster *proxysqlv1alpha1.ProxySQLCluster, scheme *runtime.Scheme, pw Passwords) *Builder {
	return &Builder{
		Cluster: cluster,
		Scheme:  scheme,
		Spec:    DefaultedSpec(cluster),
		Pw:      pw,
	}
}

// Name returns the cluster's metadata.name, used as the prefix for all
// owned objects.
func (b *Builder) Name() string { return b.Cluster.Name }

// Namespace returns the cluster's namespace.
func (b *Builder) Namespace() string { return b.Cluster.Namespace }

// HeadlessName returns the name of the headless Service used as the
// StatefulSet's serviceName.
func (b *Builder) HeadlessName() string { return b.Cluster.Name + "-headless" }

// SecretName returns the name of the Secret holding admin/radmin/monitor
// passwords. Honors AuthSpec.SecretName if set; otherwise defaults to
// the cluster name.
func (b *Builder) SecretName() string {
	if b.Spec.Auth.SecretName != "" {
		return b.Spec.Auth.SecretName
	}
	return b.Cluster.Name
}

// SecretKeys returns the AuthKeys with defaults applied.
func (b *Builder) SecretKeys() proxysqlv1alpha1.AuthKeys {
	k := b.Spec.Auth.Keys
	if k.AdminPassword == "" {
		k.AdminPassword = SecretKeyAdminPassword
	}
	if k.RadminPassword == "" {
		k.RadminPassword = SecretKeyRadminPassword
	}
	if k.MonitorPassword == "" {
		k.MonitorPassword = SecretKeyMonitorPassword
	}
	return k
}

// Image returns the fully qualified container image string.
func (b *Builder) Image() string {
	return b.Spec.Image.Repository + ":" + b.Spec.Image.Tag
}

// ManagesAuthSecret reports whether the operator should own (create and
// maintain) the auth Secret, vs. consuming an externally managed one.
func (b *Builder) ManagesAuthSecret() bool {
	return b.Spec.Auth.SecretName == ""
}

// Labels returns the standard label set for owned objects.
func (b *Builder) Labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "proxysql",
		"app.kubernetes.io/instance":   b.Cluster.Name,
		"app.kubernetes.io/component":  "proxysql-cluster",
		"app.kubernetes.io/managed-by": "proxysql-operator",
		"proxysql.com/cluster":         b.Cluster.Name,
	}
}

// SelectorLabels returns the subset of labels used for pod selectors. Must
// stay stable across upgrades; do not add fields here.
func (b *Builder) SelectorLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "proxysql",
		"app.kubernetes.io/instance": b.Cluster.Name,
		"proxysql.com/cluster":       b.Cluster.Name,
	}
}

// DefaultedSpec returns a copy of the cluster spec with operator defaults
// applied. Kubebuilder API-server defaults handle most cases, but defaulting
// inline lets the operator behave the same when running against older API
// servers or when fields are left at zero values.
func DefaultedSpec(c *proxysqlv1alpha1.ProxySQLCluster) proxysqlv1alpha1.ProxySQLClusterSpec {
	spec := *c.Spec.DeepCopy()

	if spec.Replicas == nil {
		v := int32(3)
		spec.Replicas = &v
	}
	if spec.Image.Repository == "" {
		spec.Image.Repository = DefaultProxySQLImage
	}
	if spec.Image.Tag == "" {
		spec.Image.Tag = DefaultProxySQLTag
	}
	if spec.Image.PullPolicy == "" {
		spec.Image.PullPolicy = corev1.PullIfNotPresent
	}

	// Protocol defaulting. An explicitly set Enabled always wins; when nil,
	// admin/mysql default to on and pgsql/web default to off, with a
	// non-zero port implying enabled. Enabled is always non-nil afterwards.

	// Admin port: always enabled, default 6032.
	spec.Protocols.Admin.Enabled = boolPtr(true)
	if spec.Protocols.Admin.Port == 0 {
		spec.Protocols.Admin.Port = DefaultAdminPort
	}

	// MySQL: enabled by default, default port 6033.
	if spec.Protocols.MySQL.Enabled == nil {
		spec.Protocols.MySQL.Enabled = boolPtr(true)
	}
	if spec.Protocols.MySQL.IsEnabled() && spec.Protocols.MySQL.Port == 0 {
		spec.Protocols.MySQL.Port = DefaultMySQLPort
	}

	// PostgreSQL: disabled by default; a non-zero port implies enabled.
	if spec.Protocols.PostgreSQL.Enabled == nil {
		spec.Protocols.PostgreSQL.Enabled = boolPtr(spec.Protocols.PostgreSQL.Port != 0)
	}
	if spec.Protocols.PostgreSQL.IsEnabled() && spec.Protocols.PostgreSQL.Port == 0 {
		spec.Protocols.PostgreSQL.Port = DefaultPostgreSQLPort
	}

	// Web UI: disabled by default; a non-zero port implies enabled.
	if spec.Protocols.Web.Enabled == nil {
		spec.Protocols.Web.Enabled = boolPtr(spec.Protocols.Web.Port != 0)
	}
	if spec.Protocols.Web.IsEnabled() && spec.Protocols.Web.Port == 0 {
		spec.Protocols.Web.Port = DefaultWebPort
	}

	// Persistence default: enabled, 1Gi.
	if spec.Persistence.Enabled == nil {
		t := true
		spec.Persistence.Enabled = &t
	}
	if *spec.Persistence.Enabled && spec.Persistence.Size.IsZero() {
		spec.Persistence.Size = resource.MustParse(DefaultPersistenceSize)
	}
	if *spec.Persistence.Enabled && len(spec.Persistence.AccessModes) == 0 {
		spec.Persistence.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	// Auth key defaults.
	if spec.Auth.Keys.AdminPassword == "" {
		spec.Auth.Keys.AdminPassword = SecretKeyAdminPassword
	}
	if spec.Auth.Keys.RadminPassword == "" {
		spec.Auth.Keys.RadminPassword = SecretKeyRadminPassword
	}
	if spec.Auth.Keys.MonitorPassword == "" {
		spec.Auth.Keys.MonitorPassword = SecretKeyMonitorPassword
	}

	// Metrics defaults: on by default.
	if spec.Metrics.Enabled == nil {
		t := true
		spec.Metrics.Enabled = &t
	}
	if spec.Metrics.Port == 0 {
		spec.Metrics.Port = DefaultMetricsPort
	}

	// PodDisruptionBudget default: on (the PDB is still omitted when replicas<=1).
	if spec.PodDisruptionBudget.Enabled == nil {
		t := true
		spec.PodDisruptionBudget.Enabled = &t
	}

	// Logging sidecar defaults (only when the sub-spec is present; nil means
	// the sidecar is off and stays nil).
	if spec.Logging != nil {
		defaultLogging(spec.Logging, c.Name)
	}

	// PSA-restricted-compatible default security contexts.
	if spec.PodSecurityContext == nil {
		nonRoot := true
		uid := int64(999)
		gid := int64(999)
		spec.PodSecurityContext = &corev1.PodSecurityContext{
			RunAsNonRoot: &nonRoot,
			RunAsUser:    &uid,
			RunAsGroup:   &gid,
			FSGroup:      &gid,
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		}
	}
	if spec.ContainerSecurityContext == nil {
		allowPriv := false
		readOnlyRoot := true
		spec.ContainerSecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: &allowPriv,
			ReadOnlyRootFilesystem:   &readOnlyRoot,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	}

	return spec
}

// defaultLogging applies the documented defaults to a non-nil LoggingSpec:
// stdout sink, pinned Fluent Bit image, 1Gi buffer, 50m/64Mi requests with
// 200m/128Mi limits, and per-sink fallbacks (S3 prefix, HTTP port/URI).
func defaultLogging(l *proxysqlv1alpha1.LoggingSpec, clusterName string) {
	if l.SinkType == "" {
		l.SinkType = "stdout"
	}
	if l.Image == "" {
		l.Image = DefaultFluentBitImage
	}
	if l.BufferSize.IsZero() {
		l.BufferSize = resource.MustParse(DefaultLogBufferSize)
	}
	if l.Resources.Requests == nil {
		l.Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		}
	}
	if l.Resources.Limits == nil {
		l.Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		}
	}
	if l.S3 != nil && l.S3.Prefix == "" {
		l.S3.Prefix = "/proxysql/" + clusterName
	}
	if l.HTTP != nil {
		if l.HTTP.Port == 0 {
			if l.HTTP.TLS {
				l.HTTP.Port = 443
			} else {
				l.HTTP.Port = 80
			}
		}
		if l.HTTP.URI == "" {
			l.HTTP.URI = "/"
		}
	}
}
