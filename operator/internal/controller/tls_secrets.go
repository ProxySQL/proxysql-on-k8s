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
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/tlsutil"
)

// reasonTLSSecretError is the Degraded reason for any TLS Secret
// resolution failure (missing Secret, missing key, cert-manager CRD
// absent, issuance failure). Non-wedging: same contract as
// ExternalServiceError — the reconcile continues, the error requeues.
const reasonTLSSecretError = "TLSSecretError"

// tlsCADuration is the operator-minted self-signed CA's lifetime (tier 3).
// Deliberately much longer than serving-cert lifetimes: the CA is
// preserved across reconciles so clients pinning it don't churn.
const tlsCADuration = 5 * 365 * 24 * time.Hour

// tlsRequiredKeys are the keys the resolved serving-cert Secret MUST carry
// before any TLS wiring reaches the StatefulSet. ca.crt is required even
// for tier 2 — a CA-less cert-manager issuer omits it, and the datadir
// symlink delivery (proxysql-ca.pem) needs a real file behind it.
var tlsRequiredKeys = []string{"tls.crt", "tls.key", "ca.crt"}

// ensureTLSSecrets resolves the frontend/admin serving-cert Secret per the
// three-tier precedence (user Secret > cert-manager issuerRef > operator
// self-signed) and flips the Builder's TLS mount input (TLSMountSecret)
// ONLY after validating that the resolved Secret exists with non-empty
// tls.crt, tls.key AND ca.crt.
//
// Validate-and-hold: the kubelet is never the validator. On any resolution
// failure this returns ready=false plus the error; the caller must then
// run holdTLSLastGood so the StatefulSet template never references a
// Secret the kubelet cannot satisfy, surface Degraded=TLSSecretError, and
// requeue — without wedging the rest of the reconcile.
//
//   - Tier 1 (spec.tls.secretName): the USER's Secret is validated and
//     mounted directly — never copied into an operator-managed Secret.
//   - Tier 2 (spec.tls.issuerRef): an unstructured cert-manager.io/v1
//     Certificate named <cluster>-tls is ensured (SANs from
//     tlsutil.SANsFor, duration/renewBefore from spec), then the Secret
//     cert-manager writes is validated. Missing cert-manager CRDs surface
//     as a TLSSecretError degrade, not a crash (ServiceMonitor precedent).
//   - Tier 3: a self-signed CA is minted into <cluster>-tls-ca (preserved
//     across reconciles) and a serving cert issued into <cluster>-tls,
//     reissued when absent, inside the renewal window, no longer chained
//     to the CA, or no longer covering the desired SAN set.
//
// A leftover tier-2 Certificate is garbage-collected whenever tier 2 is
// not the selected tier — otherwise cert-manager and the operator (or the
// user's Secret) would fight over the serving Secret's content.
func (r *ProxySQLClusterReconciler) ensureTLSSecrets(ctx context.Context, cluster *proxysqlv1alpha1.ProxySQLCluster, b *builders.Builder) (bool, error) {
	if !b.Spec.TLSEnabled() {
		// Disabled-but-present spec: still GC a tier-2 Certificate so
		// cert-manager stops maintaining an unused Secret. A fully absent
		// spec.tls (the overwhelmingly common case) never touches the
		// cert-manager API group at all.
		if b.Spec.TLS != nil {
			r.cleanupTLSCertificate(ctx, cluster, b.TLSSecretName())
		}
		return false, nil
	}

	tls := b.Spec.TLS
	switch {
	case tls.SecretName != "": // tier 1
		r.cleanupTLSCertificate(ctx, cluster, b.TLSSecretName())
		if err := r.validateTLSSecret(ctx, tls.SecretName, b.Namespace()); err != nil {
			return false, err
		}
		b.TLSMountSecret = tls.SecretName
		return true, nil

	case tls.IssuerRef != nil && tls.IssuerRef.Name != "": // tier 2
		if err := r.ensureTLSCertificate(ctx, cluster, b.Certificate()); err != nil {
			return false, err
		}
		if err := r.validateTLSSecret(ctx, b.TLSSecretName(), b.Namespace()); err != nil {
			return false, fmt.Errorf("waiting for cert-manager issuance: %w", err)
		}
		b.TLSMountSecret = b.TLSSecretName()
		return true, nil

	default: // tier 3
		r.cleanupTLSCertificate(ctx, cluster, b.TLSSecretName())
		if err := r.ensureSelfSignedTLS(ctx, cluster, b); err != nil {
			return false, err
		}
		b.TLSMountSecret = b.TLSSecretName()
		return true, nil
	}
}

// validateTLSSecret GETs the resolved Secret and requires every
// tlsRequiredKeys entry to be present and non-empty. The returned error
// names the Secret and, when applicable, the exact missing key — it
// becomes the Degraded=TLSSecretError message.
func (r *ProxySQLClusterReconciler) validateTLSSecret(ctx context.Context, name, namespace string) error {
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("tls secret %q not found", name)
		}
		return fmt.Errorf("get tls secret %q: %w", name, err)
	}
	for _, k := range tlsRequiredKeys {
		if len(sec.Data[k]) == 0 {
			return fmt.Errorf("tls secret %q is missing key %q", name, k)
		}
	}
	return nil
}

// holdTLSLastGood implements the hold half of validate-and-hold after a
// resolution failure: it inspects the EXISTING StatefulSet to decide what
// TLS state the template may carry this reconcile.
//
//   - Previously wired (the tls-init container and tls volume are present
//     in the live template): keep rendering TLS with the last-good mount
//     Secret — running pods keep serving with the kubelet-cached material,
//     and no template the kubelet can't satisfy is ever pushed.
//   - Never wired (no StatefulSet, or one without TLS wiring): drop the
//     Builder's TLS spec so this pass renders WITHOUT any TLS wiring
//     (volume, init container, backend cnf variables).
func (r *ProxySQLClusterReconciler) holdTLSLastGood(ctx context.Context, b *builders.Builder) error {
	var ss appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Name: b.Name(), Namespace: b.Namespace()}, &ss)
	if apierrors.IsNotFound(err) {
		b.Spec.TLS = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("get statefulset for TLS hold: %w", err)
	}

	lastGood := ""
	for _, v := range ss.Spec.Template.Spec.Volumes {
		if v.Name == builders.TLSVolumeName && v.Secret != nil {
			lastGood = v.Secret.SecretName
		}
	}
	wired := false
	for _, c := range ss.Spec.Template.Spec.InitContainers {
		if c.Name == builders.TLSInitContainerName {
			wired = true
		}
	}

	if wired && lastGood != "" {
		b.TLSMountSecret = lastGood
		return nil
	}
	b.Spec.TLS = nil
	return nil
}

// ensureTLSCertificate creates or updates the tier-2 cert-manager
// Certificate. Any failure — most commonly the cert-manager CRDs not being
// installed — comes back as an error the caller degrades on
// (TLSSecretError), never a crash: the object is unstructured precisely so
// the operator carries no cert-manager scheme dependency.
func (r *ProxySQLClusterReconciler) ensureTLSCertificate(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(builders.CertificateGVK)
	existing.SetName(desired.GetName())
	existing.SetNamespace(desired.GetNamespace())

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.SetLabels(desired.GetLabels())
		existing.Object["spec"] = desired.Object["spec"]
		return controllerutil.SetControllerReference(owner, existing, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("cert-manager Certificate %q: %v (is cert-manager installed?)", desired.GetName(), err)
	}
	return nil
}

// cleanupTLSCertificate best-effort deletes an operator-owned tier-2
// Certificate when tier 2 is no longer selected, so cert-manager stops
// rewriting the serving Secret out from under the active tier. Errors —
// including the no-matching-CRD case on clusters without cert-manager —
// are ignored by design.
func (r *ProxySQLClusterReconciler) cleanupTLSCertificate(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, name string) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(builders.CertificateGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: owner.Namespace}, existing); err != nil {
		return
	}
	if !metav1.IsControlledBy(existing, owner) {
		return
	}
	logf.FromContext(ctx).Info("deleting tier-2 cert-manager Certificate (tier no longer selected)", "certificate", name)
	_ = r.Delete(ctx, existing)
}

// ensureSelfSignedTLS maintains the tier-3 PKI: the CA Secret
// (<cluster>-tls-ca, preserved across reconciles like the operator-managed
// auth Secret) and the serving-cert Secret (<cluster>-tls). The CA is
// minted only when absent/invalid or when it could no longer outlive a
// freshly issued serving cert (duration+renewBefore before CA expiry); the
// serving cert is (re)issued per servingReissueNeeded. Both Secrets are
// shaped by construction — no separate validation pass is needed.
func (r *ProxySQLClusterReconciler) ensureSelfSignedTLS(ctx context.Context, cluster *proxysqlv1alpha1.ProxySQLCluster, b *builders.Builder) error {
	duration := b.Spec.TLS.Duration.Duration
	renewBefore := b.Spec.TLS.RenewBefore.Duration

	// --- CA ---
	caSec := &corev1.Secret{}
	caSec.Name = b.TLSCASecretName()
	caSec.Namespace = b.Namespace()
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, caSec, func() error {
		caSec.Labels = b.Labels()
		if caSec.CreationTimestamp.IsZero() {
			caSec.Type = corev1.SecretTypeTLS
		}
		// Preserve an existing CA unless it is malformed or would expire
		// before a serving cert issued today reaches its own renewal
		// window (a serving cert must never outlive its CA).
		if len(caSec.Data["tls.crt"]) > 0 && len(caSec.Data["tls.key"]) > 0 &&
			!tlsutil.NeedsRenewal(caSec.Data["tls.crt"], duration+renewBefore) {
			return controllerutil.SetControllerReference(cluster, caSec, r.Scheme)
		}
		caCrt, caKey, err := tlsutil.NewCA("proxysql-ca-"+b.Name(), tlsCADuration)
		if err != nil {
			return fmt.Errorf("minting self-signed CA: %w", err)
		}
		caSec.Data = map[string][]byte{"tls.crt": caCrt, "tls.key": caKey, "ca.crt": caCrt}
		return controllerutil.SetControllerReference(cluster, caSec, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure CA secret %q: %w", b.TLSCASecretName(), err)
	}

	// --- Serving cert ---
	sans := tlsutil.SANsFor(b.Name(), b.Namespace(), b.Spec.TLS.ExtraSANs)
	srvSec := &corev1.Secret{}
	srvSec.Name = b.TLSSecretName()
	srvSec.Namespace = b.Namespace()
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, srvSec, func() error {
		srvSec.Labels = b.Labels()
		if srvSec.CreationTimestamp.IsZero() {
			srvSec.Type = corev1.SecretTypeTLS
		}
		if !servingReissueNeeded(srvSec.Data, caSec.Data["tls.crt"], sans, renewBefore) {
			return controllerutil.SetControllerReference(cluster, srvSec, r.Scheme)
		}
		crt, key, err := tlsutil.IssueServing(caSec.Data["tls.crt"], caSec.Data["tls.key"], sans, duration)
		if err != nil {
			return fmt.Errorf("issuing serving certificate: %w", err)
		}
		srvSec.Data = map[string][]byte{"tls.crt": crt, "tls.key": key, "ca.crt": caSec.Data["tls.crt"]}
		return controllerutil.SetControllerReference(cluster, srvSec, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure serving-cert secret %q: %w", b.TLSSecretName(), err)
	}
	return nil
}

// servingReissueNeeded reports whether the tier-3 serving Secret's content
// must be (re)issued: any required key missing, the bundled ca.crt no
// longer the current CA (CA rotation), the cert inside its renewal window
// (or unparseable), or the desired SAN set no longer covered (extraSANs
// edits, cluster renames can't happen but namespace-shaped SANs are cheap
// to recheck).
func servingReissueNeeded(data map[string][]byte, caCert []byte, sans []string, renewBefore time.Duration) bool {
	for _, k := range tlsRequiredKeys {
		if len(data[k]) == 0 {
			return true
		}
	}
	if !bytes.Equal(data["ca.crt"], caCert) {
		return true
	}
	if tlsutil.NeedsRenewal(data["tls.crt"], renewBefore) {
		return true
	}
	return !certCoversSANs(data["tls.crt"], sans)
}

// certCoversSANs reports whether every desired SAN appears verbatim in the
// certificate's DNS or IP SANs. Literal comparison, no wildcard expansion:
// the operator issues wildcard entries literally, so equality is exact.
// Extra SANs on the certificate are fine (superset is a pass).
func certCoversSANs(certPEM []byte, sans []string) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}

	dns := make(map[string]bool, len(cert.DNSNames))
	for _, d := range cert.DNSNames {
		dns[d] = true
	}
	ips := make(map[string]bool, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		ips[ip.String()] = true
	}

	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			if !ips[ip.String()] {
				return false
			}
			continue
		}
		if !dns[s] {
			return false
		}
	}
	return true
}
