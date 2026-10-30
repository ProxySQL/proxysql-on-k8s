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

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

func TestConfigToDesiredRejectsMonitorVariables(t *testing.T) {
	r := &ProxySQLConfigReconciler{}
	for _, tc := range []struct {
		field string
		key   string
		set   func(*proxysqlv1alpha1.ProxySQLConfig)
	}{
		{field: "adminVariables", key: "mysql-monitor_username", set: func(c *proxysqlv1alpha1.ProxySQLConfig) {
			c.Spec.AdminVariables = map[string]string{"mysql-monitor_username": "other"}
		}},
		{field: "adminVariables", key: "pgsql-monitor_password", set: func(c *proxysqlv1alpha1.ProxySQLConfig) {
			c.Spec.AdminVariables = map[string]string{"pgsql-monitor_password": "secret-value"}
		}},
		{field: "mysqlVariables", key: "mysql-monitor_username", set: func(c *proxysqlv1alpha1.ProxySQLConfig) {
			c.Spec.MySQLVariables = map[string]string{"mysql-monitor_username": "other"}
		}},
		{field: "mysqlVariables", key: "mysql-monitor_password", set: func(c *proxysqlv1alpha1.ProxySQLConfig) {
			c.Spec.MySQLVariables = map[string]string{"mysql-monitor_password": "secret-value"}
		}},
		{field: "mysqlVariables", key: "pgsql-monitor_password", set: func(c *proxysqlv1alpha1.ProxySQLConfig) {
			c.Spec.MySQLVariables = map[string]string{"pgsql-monitor_password": "secret-value"}
		}},
		{field: "pgsqlVariables", key: "pgsql-monitor_username", set: func(c *proxysqlv1alpha1.ProxySQLConfig) {
			c.Spec.PostgreSQLVariables = map[string]string{"pgsql-monitor_username": "other"}
		}},
		{field: "pgsqlVariables", key: "pgsql-monitor_password", set: func(c *proxysqlv1alpha1.ProxySQLConfig) {
			c.Spec.PostgreSQLVariables = map[string]string{"pgsql-monitor_password": "secret-value"}
		}},
		{field: "pgsqlVariables", key: "mysql-monitor_username", set: func(c *proxysqlv1alpha1.ProxySQLConfig) {
			c.Spec.PostgreSQLVariables = map[string]string{"mysql-monitor_username": "other"}
		}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			cfg := &proxysqlv1alpha1.ProxySQLConfig{}
			tc.set(cfg)
			_, err := r.configToDesired(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("reserved variable: got %v, want field and key without value", err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("error disclosed the supplied credential")
			}
		})
	}
	valid := &proxysqlv1alpha1.ProxySQLConfig{}
	valid.Spec.MySQLVariables = map[string]string{"mysql-monitor_enabled": "false"}
	if _, err := r.configToDesired(context.Background(), valid); err != nil {
		t.Fatalf("non-credential monitor setting must remain usable: %v", err)
	}
}

func TestConfigReconcileRejectsMonitorVariableWithoutApplying(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := proxysqlv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pxc", Namespace: "default"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pxc", Namespace: "default"},
		Data: map[string][]byte{
			"admin-password": []byte("a"), "radmin-password": []byte("r"), "monitor-password": []byte("m"),
		},
	}
	cfg := &proxysqlv1alpha1.ProxySQLConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
			ClusterRef:     corev1.LocalObjectReference{Name: "pxc"},
			MySQLVariables: map[string]string{"mysql-monitor_password": "do-not-apply"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, secret, cfg).
		WithStatusSubresource(&proxysqlv1alpha1.ProxySQLConfig{}).
		Build()
	r := &ProxySQLConfigReconciler{Client: cl, Scheme: scheme}
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "cfg", Namespace: "default"}})
	if err != nil {
		t.Fatal(err)
	}
	var got proxysqlv1alpha1.ProxySQLConfig
	if err := cl.Get(ctx, types.NamespacedName{Name: "cfg", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, cfgCondReady)
	if ready == nil || ready.Reason != "InvalidVariables" || !strings.Contains(ready.Message, "mysql-monitor_password") {
		t.Fatalf("Ready condition = %v, want InvalidVariables naming the key", ready)
	}
	if strings.Contains(ready.Message, "do-not-apply") || got.Status.LastAppliedHash != "" {
		t.Fatalf("invalid variable was disclosed or applied: Ready=%v hash=%q", ready, got.Status.LastAppliedHash)
	}
}
