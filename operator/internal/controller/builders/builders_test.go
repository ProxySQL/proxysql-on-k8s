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
	if !spec.Protocols.Admin.Enabled || spec.Protocols.Admin.Port != DefaultAdminPort {
		t.Errorf("Admin protocol should be enabled at default port, got %+v", spec.Protocols.Admin)
	}
	if !spec.Protocols.MySQL.Enabled || spec.Protocols.MySQL.Port != DefaultMySQLPort {
		t.Errorf("MySQL should default to enabled at %d, got %+v", DefaultMySQLPort, spec.Protocols.MySQL)
	}
	if spec.Protocols.PostgreSQL.Enabled {
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
	if !spec.Protocols.PostgreSQL.Enabled {
		t.Error("setting a PostgreSQL port should implicitly enable PostgreSQL")
	}
	if spec.Protocols.PostgreSQL.Port != 5555 {
		t.Errorf("explicit port should be preserved, got %d", spec.Protocols.PostgreSQL.Port)
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
		c.Spec.Protocols.PostgreSQL.Enabled = true
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

func TestRandomPassword_Length(t *testing.T) {
	p, err := RandomPassword()
	if err != nil {
		t.Fatalf("RandomPassword: %v", err)
	}
	if len(p) != 32 {
		t.Errorf("password length=%d, want 32", len(p))
	}
}
