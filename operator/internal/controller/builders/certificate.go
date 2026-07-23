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
	"net"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ProxySQL/kubernetes/operator/internal/tlsutil"
)

// CertificateGVK is the GroupVersionKind of the cert-manager.io/v1
// Certificate resource (tier 2 of the TLS resolution order).
var CertificateGVK = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "Certificate",
}

// Certificate returns the desired cert-manager Certificate for tier 2 of
// the TLS resolution order, or nil when tier 2 is not selected (TLS off,
// a user Secret referenced — tier 1 wins the precedence — or no issuerRef).
//
// Built as an unstructured object so the operator doesn't require the
// cert-manager Go types as a dependency (the ServiceMonitor precedent).
// cert-manager writes the issued certificate into the operator-managed
// serving-Secret name (TLSSecretName); the reconciler validates that
// Secret before any TLS wiring reaches the StatefulSet. All nested values
// use JSON-compatible types (map[string]any / []any) — unstructured
// DeepCopy panics on anything else.
func (b *Builder) Certificate() *unstructured.Unstructured {
	tls := b.Spec.TLS
	if !b.Spec.TLSEnabled() || tls.SecretName != "" || tls.IssuerRef == nil || tls.IssuerRef.Name == "" {
		return nil
	}

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(CertificateGVK)
	cert.SetName(b.TLSSecretName())
	cert.SetNamespace(b.Namespace())
	cert.SetLabels(b.Labels())

	dnsNames, ipAddresses := splitCertSANs(tlsutil.SANsFor(b.Name(), b.Namespace(), tls.ExtraSANs))

	kind := tls.IssuerRef.Kind
	if kind == "" {
		kind = "Issuer"
	}
	group := tls.IssuerRef.Group
	if group == "" {
		group = "cert-manager.io"
	}

	spec := map[string]any{
		"secretName": b.TLSSecretName(),
		"dnsNames":   dnsNames,
		"issuerRef": map[string]any{
			"name":  tls.IssuerRef.Name,
			"kind":  kind,
			"group": group,
		},
	}
	if len(ipAddresses) > 0 {
		spec["ipAddresses"] = ipAddresses
	}
	if tls.Duration.Duration > 0 {
		spec["duration"] = tls.Duration.Duration.String()
	}
	if tls.RenewBefore.Duration > 0 {
		spec["renewBefore"] = tls.RenewBefore.Duration.String()
	}
	cert.Object["spec"] = spec

	return cert
}

// splitCertSANs partitions a mixed SAN slice into the Certificate's
// dnsNames and ipAddresses fields, as []any for unstructured compatibility.
func splitCertSANs(sans []string) (dnsNames, ipAddresses []any) {
	for _, s := range sans {
		if net.ParseIP(s) != nil {
			ipAddresses = append(ipAddresses, s)
			continue
		}
		dnsNames = append(dnsNames, s)
	}
	return dnsNames, ipAddresses
}
