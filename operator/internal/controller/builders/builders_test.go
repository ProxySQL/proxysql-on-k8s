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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

const clusterName = "pxc"

// webPortName is the named port for the ProxySQL web stats UI.
const webPortName = "web"

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := proxysqlv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func newCluster(name string, mut ...func(*proxysqlv1alpha1.ProxySQLCluster)) *proxysqlv1alpha1.ProxySQLCluster {
	c := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
	for _, m := range mut {
		m(c)
	}
	return c
}

func TestDefaultedSpec_AppliesDefaults(t *testing.T) {
	spec := DefaultedSpec(newCluster("c"))

	if spec.Replicas == nil || *spec.Replicas != 3 {
		t.Errorf("Replicas default: got %v want 3", spec.Replicas)
	}
	if spec.Image.Repository != DefaultProxySQLImage || spec.Image.Tag != DefaultProxySQLTag {
		t.Errorf("Image default: got %s:%s", spec.Image.Repository, spec.Image.Tag)
	}
	if !spec.Protocols.Admin.IsEnabled() || spec.Protocols.Admin.Port != DefaultAdminPort {
		t.Errorf("Admin protocol should be enabled at default port, got %+v", spec.Protocols.Admin)
	}
	if !spec.Protocols.MySQL.IsEnabled() || spec.Protocols.MySQL.Port != DefaultMySQLPort {
		t.Errorf("MySQL should default to enabled at %d, got %+v", DefaultMySQLPort, spec.Protocols.MySQL)
	}
	if spec.Protocols.PostgreSQL.IsEnabled() {
		t.Errorf("PostgreSQL should default to disabled, got %+v", spec.Protocols.PostgreSQL)
	}
	if spec.PodSecurityContext == nil || spec.PodSecurityContext.RunAsNonRoot == nil || !*spec.PodSecurityContext.RunAsNonRoot {
		t.Errorf("PodSecurityContext should default to runAsNonRoot=true")
	}
	if spec.ContainerSecurityContext == nil || spec.ContainerSecurityContext.ReadOnlyRootFilesystem == nil || !*spec.ContainerSecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("ContainerSecurityContext should default to readOnlyRootFilesystem=true")
	}
}

func TestDefaultedSpec_PostgreSQLEnabledImplicitlyByPort(t *testing.T) {
	c := newCluster("c", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.PostgreSQL.Port = 5555
	})
	spec := DefaultedSpec(c)
	if !spec.Protocols.PostgreSQL.IsEnabled() {
		t.Error("setting a PostgreSQL port should implicitly enable PostgreSQL")
	}
	if spec.Protocols.PostgreSQL.Port != 5555 {
		t.Errorf("explicit port should be preserved, got %d", spec.Protocols.PostgreSQL.Port)
	}
}

func TestDefaultedSpec_MySQLExplicitFalse_StaysDisabled(t *testing.T) {
	c := newCluster("c", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.MySQL.Enabled = boolPtr(false)
	})
	spec := DefaultedSpec(c)
	if spec.Protocols.MySQL.IsEnabled() {
		t.Error("explicit mysql enabled=false must survive defaulting (#31)")
	}

	// Explicit false wins even when a port is set (port-implies-enabled only
	// applies when Enabled is nil).
	c = newCluster("c", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.MySQL.Enabled = boolPtr(false)
		c.Spec.Protocols.MySQL.Port = 3306
	})
	spec = DefaultedSpec(c)
	if spec.Protocols.MySQL.IsEnabled() {
		t.Error("explicit mysql enabled=false must win over a non-zero port")
	}

	// Regression guard: nil Enabled still defaults to on.
	spec = DefaultedSpec(newCluster("c"))
	if !spec.Protocols.MySQL.IsEnabled() || spec.Protocols.MySQL.Port != DefaultMySQLPort {
		t.Errorf("nil mysql.enabled must default to enabled at %d, got %+v", DefaultMySQLPort, spec.Protocols.MySQL)
	}
}

func TestDefaultedSpec_PgsqlWebExplicitFalseWithPort_StaysDisabled(t *testing.T) {
	c := newCluster("c", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.PostgreSQL.Enabled = boolPtr(false)
		c.Spec.Protocols.PostgreSQL.Port = 5432
		c.Spec.Protocols.Web.Enabled = boolPtr(false)
		c.Spec.Protocols.Web.Port = 6080
	})
	spec := DefaultedSpec(c)
	if spec.Protocols.PostgreSQL.IsEnabled() {
		t.Error("explicit pgsql enabled=false must win over a non-zero port")
	}
	if spec.Protocols.Web.IsEnabled() {
		t.Error("explicit web enabled=false must win over a non-zero port")
	}
}

func TestDefaultedSpec_AdminAlwaysEnabled(t *testing.T) {
	// The admin listener cannot be disabled: the operator depends on it to
	// push ProxySQLConfig. An explicit false is deliberately overridden.
	c := newCluster("c", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.Admin.Enabled = boolPtr(false)
	})
	spec := DefaultedSpec(c)
	if !spec.Protocols.Admin.IsEnabled() {
		t.Error("admin.enabled=false must be overridden: admin is always on")
	}
}

func TestBuilder_MySQLDisabled_OmittedEverywhere(t *testing.T) {
	c := newCluster("nomysql", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.MySQL.Enabled = boolPtr(false)
	})
	b := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})

	for _, p := range b.Service().Spec.Ports {
		if p.Name == "mysql" {
			t.Errorf("Service must not expose mysql port when disabled: %+v", p)
		}
	}
	for _, p := range b.StatefulSet("checksum").Spec.Template.Spec.Containers[0].Ports {
		if p.Name == "mysql" {
			t.Errorf("container must not declare mysql port when disabled: %+v", p)
		}
	}
	cnf, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	if strings.Contains(cnf, "mysql_variables") {
		t.Errorf("cnf must not contain mysql_variables when mysql is disabled:\n%s", cnf)
	}
	if ep := b.Endpoints(); ep.MySQL != "" {
		t.Errorf("MySQL endpoint should be empty when disabled, got %q", ep.MySQL)
	}
}

func TestBuilder_Names(t *testing.T) {
	b := New(newCluster(clusterName), newScheme(t), Passwords{})
	if b.Name() != clusterName {
		t.Errorf("Name=%s", b.Name())
	}
	if b.HeadlessName() != "pxc-headless" {
		t.Errorf("HeadlessName=%s", b.HeadlessName())
	}
	if b.SecretName() != clusterName {
		t.Errorf("SecretName default should match cluster name, got %s", b.SecretName())
	}
}

func TestBuilder_SecretName_HonoursExternal(t *testing.T) {
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Auth.SecretName = "byo-creds"
	})
	b := New(c, newScheme(t), Passwords{})
	if b.SecretName() != "byo-creds" {
		t.Errorf("SecretName=%s want byo-creds", b.SecretName())
	}
	if b.ManagesAuthSecret() {
		t.Error("operator should not claim ownership when SecretName is external")
	}
}

func TestBuilder_ConfigMap_ClusterSyncOnMultiReplica(t *testing.T) {
	c := newCluster(clusterName)
	b := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})

	cm, err := b.ConfigMap()
	if err != nil {
		t.Fatalf("ConfigMap: %v", err)
	}
	cnf := cm.Data["proxysql.cnf"]
	if !strings.Contains(cnf, `admin_credentials="admin:a;radmin:r"`) {
		t.Errorf("cnf missing admin_credentials\n%s", cnf)
	}
	if !strings.Contains(cnf, "cluster_username=") {
		t.Errorf("cnf should contain cluster_username when replicas>1\n%s", cnf)
	}
	if !strings.Contains(cnf, "proxysql_servers=") {
		t.Errorf("cnf should contain proxysql_servers block for replicas>1\n%s", cnf)
	}
}

func TestBuilder_ConfigMap_PostgreSQLMonitorCreds(t *testing.T) {
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.PostgreSQL.Enabled = boolPtr(true)
	})
	b := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "monitorpw"})

	cm, err := b.ConfigMap()
	if err != nil {
		t.Fatalf("ConfigMap: %v", err)
	}
	cnf := cm.Data["proxysql.cnf"]
	// The pgsql_variables block must carry monitor credentials so PostgreSQL
	// backend health checks can authenticate (verified pgsql-monitor_username/
	// password exist in ProxySQL 3.0).
	pgIdx := strings.Index(cnf, "pgsql_variables=")
	if pgIdx < 0 {
		t.Fatalf("cnf missing pgsql_variables block\n%s", cnf)
	}
	pgBlock := cnf[pgIdx:]
	if !strings.Contains(pgBlock, `monitor_username="monitor"`) {
		t.Errorf("pgsql_variables missing monitor_username\n%s", cnf)
	}
	if !strings.Contains(pgBlock, `monitor_password="monitorpw"`) {
		t.Errorf("pgsql_variables missing monitor_password\n%s", cnf)
	}
}

func TestBuilder_ConfigMap_NoClusterSyncOnSingleReplica(t *testing.T) {
	one := int32(1)
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) { c.Spec.Replicas = &one })
	b := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})

	cm, _ := b.ConfigMap()
	cnf := cm.Data["proxysql.cnf"]
	if strings.Contains(cnf, "cluster_username=") {
		t.Errorf("single-replica cnf should not contain cluster sync vars\n%s", cnf)
	}
	if strings.Contains(cnf, "proxysql_servers=") {
		t.Errorf("single-replica cnf should omit proxysql_servers\n%s", cnf)
	}
}

func TestBuilder_StatefulSet_PortsMatchEnabledProtocols(t *testing.T) {
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.PostgreSQL.Enabled = boolPtr(true)
	})
	b := New(c, newScheme(t), Passwords{})
	ss := b.StatefulSet("checksum")
	ports := ss.Spec.Template.Spec.Containers[0].Ports

	names := map[string]int32{}
	for _, p := range ports {
		names[p.Name] = p.ContainerPort
	}
	for _, want := range []string{"admin", "mysql", "pgsql", "metrics"} {
		if _, ok := names[want]; !ok {
			t.Errorf("container missing port %s: have %v", want, names)
		}
	}
	if names["admin"] != DefaultAdminPort || names["mysql"] != DefaultMySQLPort || names["pgsql"] != DefaultPostgreSQLPort {
		t.Errorf("unexpected port values: %v", names)
	}
}

func TestBuilder_StatefulSet_SetsProxysqlCommand(t *testing.T) {
	// The proxysql/proxysql image has no ENTRYPOINT, so command MUST be set to
	// the binary; otherwise Kubernetes execs args[0] ("-f") and the container
	// CrashLoops. Guard against regressing back to args-only.
	b := New(newCluster(clusterName), newScheme(t), Passwords{})
	c := b.StatefulSet("checksum").Spec.Template.Spec.Containers[0]
	if len(c.Command) == 0 || c.Command[0] != "proxysql" {
		t.Errorf("container command must start with \"proxysql\", got %v", c.Command)
	}
	if len(c.Args) == 0 || c.Args[0] != "-f" {
		t.Errorf("container args should begin with -f, got %v", c.Args)
	}
}

func TestBuilder_StatefulSet_PersistenceOff_UsesEmptyDir(t *testing.T) {
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		f := false
		c.Spec.Persistence.Enabled = &f
	})
	b := New(c, newScheme(t), Passwords{})
	ss := b.StatefulSet("checksum")
	if len(ss.Spec.VolumeClaimTemplates) != 0 {
		t.Errorf("persistence disabled but VolumeClaimTemplates present: %d", len(ss.Spec.VolumeClaimTemplates))
	}
	found := false
	for _, v := range ss.Spec.Template.Spec.Volumes {
		if v.Name == "data" && v.EmptyDir != nil {
			found = true
		}
	}
	if !found {
		t.Errorf("persistence disabled should fall back to an emptyDir data volume")
	}
}

func TestBuilder_StatefulSet_PersistenceOn_HasPVC(t *testing.T) {
	c := newCluster(clusterName) // defaults: persistence size 1Gi
	b := New(c, newScheme(t), Passwords{})
	ss := b.StatefulSet("checksum")
	if len(ss.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("want 1 volumeClaimTemplate, got %d", len(ss.Spec.VolumeClaimTemplates))
	}
	pvc := ss.Spec.VolumeClaimTemplates[0]
	if pvc.Name != "data" {
		t.Errorf("PVC name=%s, want data", pvc.Name)
	}
	if pvc.Spec.Resources.Requests.Storage().String() != "1Gi" {
		t.Errorf("PVC storage=%s, want 1Gi", pvc.Spec.Resources.Requests.Storage().String())
	}
}

func TestBuilder_HeadlessService_IsHeadless(t *testing.T) {
	b := New(newCluster(clusterName), newScheme(t), Passwords{})
	svc := b.HeadlessService()
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("HeadlessService ClusterIP=%q, want None", svc.Spec.ClusterIP)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("HeadlessService should publish not-ready addresses for bootstrap")
	}
	// Headless should not expose metrics — operator scrapes via the regular Service.
	for _, p := range svc.Spec.Ports {
		if p.Name == "metrics" {
			t.Errorf("headless service should not include metrics port")
		}
	}
}

func TestBuilder_PDB_DisabledOrSingleReplica_ReturnsNil(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*proxysqlv1alpha1.ProxySQLCluster)
	}{
		{
			name: "explicitly disabled",
			mut: func(c *proxysqlv1alpha1.ProxySQLCluster) {
				f := false
				c.Spec.PodDisruptionBudget.Enabled = &f
			},
		},
		{
			name: "single replica",
			mut: func(c *proxysqlv1alpha1.ProxySQLCluster) {
				one := int32(1)
				c.Spec.Replicas = &one
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(clusterName, tc.mut)
			b := New(c, newScheme(t), Passwords{})
			if pdb := b.PodDisruptionBudget(); pdb != nil {
				t.Errorf("expected nil PDB, got %+v", pdb)
			}
		})
	}
}

func TestBuilder_PDB_DefaultPolicyKeepsAllButOne(t *testing.T) {
	c := newCluster(clusterName) // defaults: replicas=3, PDB enabled
	b := New(c, newScheme(t), Passwords{})
	pdb := b.PodDisruptionBudget()
	if pdb == nil {
		t.Fatal("expected non-nil PDB for replicas=3")
	}
	want := intstr.FromInt32(2) // 3 - 1
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.String() != want.String() {
		t.Errorf("MinAvailable=%v, want %v", pdb.Spec.MinAvailable, want)
	}
}

func TestBuilder_Labels_AlwaysIncludeClusterSelector(t *testing.T) {
	b := New(newCluster(clusterName), newScheme(t), Passwords{})
	for _, lbls := range []map[string]string{b.Labels(), b.SelectorLabels()} {
		if lbls["proxysql.com/cluster"] != clusterName {
			t.Errorf("missing proxysql.com/cluster label, have %v", lbls)
		}
		if lbls["app.kubernetes.io/instance"] != clusterName {
			t.Errorf("missing app.kubernetes.io/instance, have %v", lbls)
		}
	}
}

func TestBuilder_ServiceMonitor_DisabledByDefault(t *testing.T) {
	b := New(newCluster(clusterName), newScheme(t), Passwords{})
	if sm := b.ServiceMonitor(); sm != nil {
		t.Errorf("ServiceMonitor should be nil when ServiceMonitor.Enabled is false; got %+v", sm)
	}
}

func TestBuilder_ServiceMonitor_OnlyEmittedWhenMetricsAlsoEnabled(t *testing.T) {
	f := false
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Metrics.Enabled = &f
		c.Spec.Metrics.ServiceMonitor.Enabled = true
	})
	b := New(c, newScheme(t), Passwords{})
	if sm := b.ServiceMonitor(); sm != nil {
		t.Errorf("ServiceMonitor should be nil when metrics are disabled, even if SM.Enabled=true")
	}
}

func TestBuilder_ServiceMonitor_Shape(t *testing.T) {
	c := newCluster(clusterName, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Metrics.ServiceMonitor.Enabled = true
		c.Spec.Metrics.ServiceMonitor.Interval = "15s"
		c.Spec.Metrics.ServiceMonitor.Labels = map[string]string{"release": "prometheus"}
	})
	b := New(c, newScheme(t), Passwords{})
	sm := b.ServiceMonitor()
	if sm == nil {
		t.Fatal("expected non-nil ServiceMonitor")
	}
	if sm.GetKind() != "ServiceMonitor" || sm.GetAPIVersion() != "monitoring.coreos.com/v1" {
		t.Errorf("wrong GVK: %s/%s", sm.GetAPIVersion(), sm.GetKind())
	}
	if sm.GetLabels()["release"] != "prometheus" {
		t.Errorf("user-supplied labels not merged: %v", sm.GetLabels())
	}
	spec, ok := sm.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type: %T", sm.Object["spec"])
	}
	endpoints, ok := spec["endpoints"].([]any)
	if !ok || len(endpoints) != 1 {
		t.Fatalf("endpoints malformed: %v", spec["endpoints"])
	}
	ep := endpoints[0].(map[string]any)
	if ep["port"] != "metrics" || ep["interval"] != "15s" || ep["path"] != "/metrics" {
		t.Errorf("endpoint wrong: %v", ep)
	}
}

func TestDefaultedSpec_WebUI(t *testing.T) {
	// disabled by default
	spec := DefaultedSpec(newCluster("c"))
	if spec.Protocols.Web.IsEnabled() {
		t.Errorf("web UI must default to disabled")
	}

	// enabled without port => default 6080
	c := newCluster("c", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.Web.Enabled = boolPtr(true)
	})
	spec = DefaultedSpec(c)
	if spec.Protocols.Web.Port != DefaultWebPort {
		t.Errorf("web port = %d, want %d", spec.Protocols.Web.Port, DefaultWebPort)
	}

	// non-zero port implies enabled (same convention as MySQL/PostgreSQL)
	c = newCluster("c", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.Web.Port = 6081
	})
	spec = DefaultedSpec(c)
	if !spec.Protocols.Web.IsEnabled() || spec.Protocols.Web.Port != 6081 {
		t.Errorf("port-implies-enabled failed: %+v", spec.Protocols.Web)
	}
}

func TestBootstrapCnf_WebUI(t *testing.T) {
	c := newCluster("web-test", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.Web.Enabled = boolPtr(true)
	})
	b := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	cnf, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	for _, want := range []string{"web_enabled=true", "web_port=6080"} {
		if !strings.Contains(cnf, want) {
			t.Errorf("cnf missing %q:\n%s", want, cnf)
		}
	}
	// and absent when disabled
	b2 := New(newCluster("web-off"), newScheme(t), Passwords{})
	cnf2, err := b2.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	if strings.Contains(cnf2, "web_enabled") {
		t.Errorf("cnf must not mention web_enabled when disabled:\n%s", cnf2)
	}
}

func TestServicePorts_WebUI(t *testing.T) {
	c := newCluster("web-svc", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.Web.Enabled = boolPtr(true)
	})
	b := New(c, newScheme(t), Passwords{})
	svc := b.Service()
	found := false
	for _, p := range svc.Spec.Ports {
		if p.Name == webPortName && p.Port == DefaultWebPort {
			found = true
		}
	}
	if !found {
		t.Errorf("regular Service missing web port: %+v", svc.Spec.Ports)
	}
	// headless never exposes web (same policy as metrics)
	for _, p := range b.HeadlessService().Spec.Ports {
		if p.Name == webPortName {
			t.Errorf("headless Service must not expose web")
		}
	}
}

func TestStatefulSet_WebUIContainerPort(t *testing.T) {
	c := newCluster("web-sts", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.Web.Enabled = boolPtr(true)
	})
	b := New(c, newScheme(t), Passwords{})
	sts := b.StatefulSet("checksum")
	found := false
	for _, p := range sts.Spec.Template.Spec.Containers[0].Ports {
		if p.Name == webPortName && p.ContainerPort == DefaultWebPort {
			found = true
		}
	}
	if !found {
		t.Errorf("container missing web port: %+v", sts.Spec.Template.Spec.Containers[0].Ports)
	}

	// absent when disabled
	b2 := New(newCluster("web-sts-off"), newScheme(t), Passwords{})
	for _, p := range b2.StatefulSet("checksum").Spec.Template.Spec.Containers[0].Ports {
		if p.Name == webPortName {
			t.Errorf("container must not declare web port when disabled")
		}
	}
}

func TestEndpoints(t *testing.T) {
	c := newCluster("ep", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Namespace = "ns1"
		c.Spec.Protocols.Web.Enabled = boolPtr(true)
	})
	b := New(c, newScheme(t), Passwords{})
	got := b.Endpoints()
	if got.MySQL != "ep.ns1.svc:6033" { // mysql enabled by default
		t.Errorf("MySQL endpoint = %q", got.MySQL)
	}
	if got.Admin != "ep.ns1.svc:6032" {
		t.Errorf("Admin endpoint = %q", got.Admin)
	}
	if got.Web != "ep.ns1.svc:6080" {
		t.Errorf("Web endpoint = %q", got.Web)
	}
	if got.Metrics != "ep.ns1.svc:6070" { // metrics on by default
		t.Errorf("Metrics endpoint = %q", got.Metrics)
	}
	if got.PostgreSQL != "" { // pgsql off by default
		t.Errorf("PostgreSQL endpoint should be empty, got %q", got.PostgreSQL)
	}
}

func TestEndpoints_DisabledSurfacesEmpty(t *testing.T) {
	f := false
	c := newCluster("ep-off", func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Protocols.PostgreSQL.Enabled = boolPtr(true)
		c.Spec.Metrics.Enabled = &f
	})
	b := New(c, newScheme(t), Passwords{})
	got := b.Endpoints()
	if got.PostgreSQL != "ep-off.default.svc:6133" {
		t.Errorf("PostgreSQL endpoint = %q", got.PostgreSQL)
	}
	if got.Web != "" {
		t.Errorf("Web endpoint should be empty when disabled, got %q", got.Web)
	}
	if got.Metrics != "" {
		t.Errorf("Metrics endpoint should be empty when disabled, got %q", got.Metrics)
	}
}

func TestPasswordsFromSecret(t *testing.T) {
	const platformPass = "s3cret"
	keys := proxysqlv1alpha1.AuthKeys{
		AdminPassword:   "admin-password",
		RadminPassword:  "radmin-password",
		MonitorPassword: "monitor-password",
	}

	// Operator schema wins even if username/password is also present.
	pw, err := PasswordsFromSecret(map[string][]byte{
		"admin-password": []byte("a"), "radmin-password": []byte("r"), "monitor-password": []byte("m"),
		"username": []byte("ops"), "password": []byte("x"),
	}, keys)
	if err != nil || pw.Admin != "a" || pw.Radmin != "r" || pw.Monitor != "m" || pw.ExtraAdminUser != "" {
		t.Fatalf("operator schema: pw=%+v err=%v", pw, err)
	}

	// username/password schema.
	pw, err = PasswordsFromSecret(map[string][]byte{
		"username": []byte("platform"), "password": []byte(platformPass),
	}, keys)
	if err != nil || pw.Admin != platformPass || pw.Radmin != platformPass || pw.Monitor != platformPass {
		t.Fatalf("username/password schema: pw=%+v err=%v", pw, err)
	}
	if pw.ExtraAdminUser != "platform" || pw.ExtraAdminPassword != platformPass {
		t.Fatalf("extra admin credential not derived: %+v", pw)
	}

	// username/password schema + an explicit monitor key overrides Monitor.
	pw, err = PasswordsFromSecret(map[string][]byte{
		"username": []byte("platform"), "password": []byte(platformPass),
		"monitor-password": []byte("mon"),
	}, keys)
	if err != nil || pw.Monitor != "mon" {
		t.Fatalf("monitor key should override: pw=%+v err=%v", pw, err)
	}

	// username == radmin must NOT produce an extra credential.
	pw, _ = PasswordsFromSecret(map[string][]byte{
		"username": []byte("radmin"), "password": []byte(platformPass),
	}, keys)
	if pw.ExtraAdminUser != "" {
		t.Fatalf("radmin username must not duplicate credential: %+v", pw)
	}

	// username == admin must NOT produce an extra credential either.
	pw, _ = PasswordsFromSecret(map[string][]byte{
		"username": []byte("admin"), "password": []byte(platformPass),
	}, keys)
	if pw.ExtraAdminUser != "" {
		t.Fatalf("admin username must not duplicate credential: %+v", pw)
	}

	// Neither schema -> error naming both.
	_, err = PasswordsFromSecret(map[string][]byte{"foo": []byte("bar")}, keys)
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("expected both-schema error, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "admin-password") {
		t.Fatalf("error should name the operator-schema keys, got %v", err)
	}
}

func TestPasswordsFromSecret_Validation(t *testing.T) {
	const platformPass = "s3cret"
	keys := proxysqlv1alpha1.AuthKeys{
		AdminPassword:   "admin-password",
		RadminPassword:  "radmin-password",
		MonitorPassword: "monitor-password",
	}

	// Partial operator schema must error (naming the missing key), even when
	// username/password is also present — never silently substitute.
	_, err := PasswordsFromSecret(map[string][]byte{
		"admin-password": []byte("a"), "radmin-password": []byte("r"),
		"username": []byte("platform"), "password": []byte(platformPass),
	}, keys)
	if err == nil || !strings.Contains(err.Error(), "monitor-password") {
		t.Fatalf("partial operator schema should error naming monitor-password, got %v", err)
	}

	// Username containing cnf-breaking characters -> error.
	for _, user := range []string{`evil";`, "evil\nuser", "a b"} {
		_, err = PasswordsFromSecret(map[string][]byte{
			"username": []byte(user), "password": []byte(platformPass),
		}, keys)
		if err == nil || !strings.Contains(err.Error(), "username") {
			t.Fatalf("username %q should be rejected, got %v", user, err)
		}
	}

	// Password containing ';' (breaks admin_credentials splitting) -> error.
	_, err = PasswordsFromSecret(map[string][]byte{
		"username": []byte("platform"), "password": []byte("pa;ss"),
	}, keys)
	if err == nil || !strings.Contains(err.Error(), `"password"`) {
		t.Fatalf("password with ';' should be rejected, got %v", err)
	}

	// Operator-schema passwords are validated too.
	_, err = PasswordsFromSecret(map[string][]byte{
		"admin-password":   []byte(`a"b`),
		"radmin-password":  []byte("r"),
		"monitor-password": []byte("m"),
	}, keys)
	if err == nil || !strings.Contains(err.Error(), "admin-password") {
		t.Fatalf("operator-schema password with '\"' should be rejected, got %v", err)
	}

	// Clean username/password schema still works after validation hardening.
	pw, err := PasswordsFromSecret(map[string][]byte{
		"username": []byte("plat-form_user.1"), "password": []byte(platformPass),
	}, keys)
	if err != nil || pw.ExtraAdminUser != "plat-form_user.1" || pw.Admin != platformPass {
		t.Fatalf("clean username/password schema should pass: pw=%+v err=%v", pw, err)
	}
}

func TestBootstrapCnf_ExtraAdminCredential(t *testing.T) {
	c := newCluster("extra")
	b := New(c, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m",
		ExtraAdminUser: "platform", ExtraAdminPassword: "s3cret"})
	cnf, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cnf, `admin_credentials="admin:a;radmin:r;platform:s3cret"`) {
		t.Errorf("extra credential missing:\n%s", cnf)
	}

	// No extra credential -> the line stays in its two-account form.
	b2 := New(newCluster("plain"), newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	cnf2, err := b2.BootstrapCnf(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cnf2, `admin_credentials="admin:a;radmin:r"`) {
		t.Errorf("two-account credentials line malformed:\n%s", cnf2)
	}
}

func TestRandomPassword_Length(t *testing.T) {
	p, err := RandomPassword()
	if err != nil {
		t.Fatalf("RandomPassword: %v", err)
	}
	if len(p) != 32 {
		t.Errorf("password length=%d, want 32", len(p))
	}
}
