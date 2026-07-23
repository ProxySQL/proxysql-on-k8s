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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/tlsutil"
)

// TestEnsureTLSSecretsCertManagerCRDAbsent covers the tier-2 path on a
// cluster WITHOUT the cert-manager CRDs. envtest can't model this (the
// suite installs a Certificate CRD fixture for the happy path), so a fake
// client's interceptor returns the exact NoKindMatchError the RESTMapper
// produces for an uninstalled CRD. Contract: a TLSSecretError-worthy error
// (degrade + requeue), never a crash — and the tier-1/3 paths must shrug
// off the same condition in their Certificate garbage-collection.
func TestEnsureTLSSecretsCertManagerCRDAbsent(t *testing.T) {
	// A dedicated scheme (never the global one): registration must not
	// depend on the ginkgo suite's BeforeSuite having run first.
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := proxysqlv1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	noMatch := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"},
		SearchedVersions: []string{"v1"},
	}
	// Reject every unstructured access (only the cert-manager Certificate
	// is unstructured in this code path) with the no-CRD error.
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*unstructured.Unstructured); ok {
				return noMatch
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*unstructured.Unstructured); ok {
				return noMatch
			}
			return c.Create(ctx, obj, opts...)
		},
	}

	newCluster := func(mut func(*proxysqlv1alpha1.ProxySQLCluster)) *proxysqlv1alpha1.ProxySQLCluster {
		c := &proxysqlv1alpha1.ProxySQLCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "no-cm", Namespace: "default", UID: "uid-no-cm"},
		}
		mut(c)
		return c
	}

	validTLSData := func(t *testing.T, name string) map[string][]byte {
		t.Helper()
		caCrt, caKey, err := tlsutil.NewCA("unit-ca", 24*time.Hour)
		if err != nil {
			t.Fatalf("NewCA: %v", err)
		}
		crt, key, err := tlsutil.IssueServing(caCrt, caKey, tlsutil.SANsFor(name, "default", nil), 24*time.Hour)
		if err != nil {
			t.Fatalf("IssueServing: %v", err)
		}
		return map[string][]byte{"tls.crt": crt, "tls.key": key, "ca.crt": caCrt}
	}

	t.Run("tier 2 degrades instead of crashing", func(t *testing.T) {
		cluster := newCluster(func(c *proxysqlv1alpha1.ProxySQLCluster) {
			c.Spec.TLS = &proxysqlv1alpha1.TLSSpec{
				Enabled:   true,
				IssuerRef: &proxysqlv1alpha1.TLSIssuerRef{Name: "my-issuer"},
			}
		})
		cl := fake.NewClientBuilder().WithScheme(sch).
			WithObjects(cluster).WithInterceptorFuncs(funcs).Build()
		r := &ProxySQLClusterReconciler{Client: cl, Scheme: sch}
		b := builders.New(cluster, sch, builders.Passwords{})

		ready, err := r.ensureTLSSecrets(context.Background(), cluster, b)
		if ready {
			t.Fatal("ensureTLSSecrets must not report ready without cert-manager CRDs")
		}
		if err == nil {
			t.Fatal("ensureTLSSecrets must surface the missing cert-manager CRDs as an error")
		}
		if !strings.Contains(err.Error(), "cert-manager") {
			t.Fatalf("error should point at cert-manager, got: %v", err)
		}
		if b.TLSMountSecret != "" {
			t.Fatalf("TLSMountSecret must stay unset on failure, got %q", b.TLSMountSecret)
		}
	})

	t.Run("tier 1 still resolves; Certificate GC shrugs off the missing CRD", func(t *testing.T) {
		cluster := newCluster(func(c *proxysqlv1alpha1.ProxySQLCluster) {
			c.Spec.TLS = &proxysqlv1alpha1.TLSSpec{Enabled: true, SecretName: "user-tls"}
		})
		userSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "user-tls", Namespace: "default"},
			Data:       validTLSData(t, "no-cm"),
		}
		cl := fake.NewClientBuilder().WithScheme(sch).
			WithObjects(cluster, userSecret).WithInterceptorFuncs(funcs).Build()
		r := &ProxySQLClusterReconciler{Client: cl, Scheme: sch}
		b := builders.New(cluster, sch, builders.Passwords{})

		ready, err := r.ensureTLSSecrets(context.Background(), cluster, b)
		if err != nil {
			t.Fatalf("tier 1 must not fail on a cluster without cert-manager: %v", err)
		}
		if !ready {
			t.Fatal("tier 1 with a valid user Secret must be ready")
		}
		if b.TLSMountSecret != "user-tls" {
			t.Fatalf("tier 1 must mount the user's Secret directly, got %q", b.TLSMountSecret)
		}
	})
}
