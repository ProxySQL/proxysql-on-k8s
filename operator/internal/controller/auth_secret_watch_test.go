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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

func TestReferencedSecretWatchEnqueuesExternalAuthCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := proxysqlv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	matching := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: "one"},
		Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
			Auth: proxysqlv1alpha1.AuthSpec{SecretName: "shared-auth"},
		},
	}
	otherSecret := matching.DeepCopy()
	otherSecret.Name = "other-secret"
	otherSecret.Spec.Auth.SecretName = "unrelated-auth"
	otherNamespace := matching.DeepCopy()
	otherNamespace.Name = "other-namespace"
	otherNamespace.Namespace = "two"

	noValues := func(client.Object) []string { return nil }
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(matching, otherSecret, otherNamespace).
		WithIndex(&proxysqlv1alpha1.ProxySQLCluster{}, authSecretNameIndex, func(obj client.Object) []string {
			if name := obj.(*proxysqlv1alpha1.ProxySQLCluster).Spec.Auth.SecretName; name != "" {
				return []string{name}
			}
			return nil
		}).
		WithIndex(&proxysqlv1alpha1.ProxySQLCluster{}, tlsSecretNameIndex, noValues).
		WithIndex(&proxysqlv1alpha1.ProxySQLCluster{}, tlsBackendCASecretIndex, noValues).
		WithIndex(&proxysqlv1alpha1.ProxySQLCluster{}, tlsBackendCertSecretIndex, noValues).
		Build()
	r := &ProxySQLClusterReconciler{Client: cl, Scheme: scheme}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared-auth", Namespace: "one"}}
	reqs := r.clustersForReferencedSecret(context.Background(), secret)
	if len(reqs) != 1 || reqs[0].NamespacedName.Name != "matching" || reqs[0].NamespacedName.Namespace != "one" {
		t.Fatalf("auth Secret event enqueued %v, want one matching cluster in the same namespace", reqs)
	}
}
