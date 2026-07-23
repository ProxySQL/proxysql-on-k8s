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
	"crypto/tls"
	"crypto/x509"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
)

// radminUser is the remote-capable admin account both reconcilers dial
// with (ProxySQL hardcodes "admin" to localhost-only).
const radminUser = "radmin"

// adminTLS bundles the trust material for ROUTINE admin dials against a
// TLS-wired cluster: the CA pool from the mounted serving-cert Secret plus
// each ready pod's SAN-covered DNS identity. A nil *adminTLS means
// plaintext — TLS-off clusters dial exactly as they always have.
type adminTLS struct {
	base        *tls.Config       // RootCAs populated from the mounted Secret's ca.crt
	serverNames map[string]string // addr → per-pod DNS name (podEndpoint.ServerName)
}

// configFor returns the per-dial tls.Config for addr, or nil for plaintext.
// The connection dials the pod IP but verifies the certificate against the
// pod's DNS ServerName — pod IPs are not (and cannot be) in the SAN set;
// see podEndpoint.ServerName.
func (t *adminTLS) configFor(addr string) *tls.Config {
	if t == nil || t.base == nil {
		return nil
	}
	cfg := t.base.Clone()
	cfg.ServerName = t.serverNames[addr]
	return cfg
}

// dial opens the admin client for addr with the cluster's trust config
// (plaintext when t is nil).
func (t *adminTLS) dial(addr, user, pass string) (*proxysqlclient.Client, error) {
	return proxysqlclient.NewWithTLS(addr, user, pass, t.configFor(addr))
}

// adminDialTLS builds the adminTLS trust bundle for a cluster, or nil
// (plaintext) when its PODS are not TLS-wired. "Wired" is read from the
// LIVE StatefulSet's tls volume — not from spec.tls — because the dials
// must match what the running pods actually serve:
//
//   - spec.tls freshly enabled, rollout not started: pods still serve the
//     ProxySQL autogen cert the operator has no trust anchor for →
//     plaintext, exactly as before the enable.
//   - validate-and-hold keeping a last-good Secret: the live volume names
//     that Secret, so the pool matches the material the pods mount.
//   - during a rotation the pods may serve the OLD leaf until RELOAD TLS
//     lands; in every tier the old leaf chains to the ca.crt bundled in
//     the CURRENT Secret except a tier-1 full-bundle swap (CA replaced
//     too) — there the dial fails as "not yet reloaded", the rotation
//     engine (which runs FIRST in the cluster reconcile, and dials
//     fingerprint-pinned rather than pool-verified) reloads the pod, and
//     the failed routine dial's requeue retries against the new cert.
func adminDialTLS(ctx context.Context, c client.Client, cluster *proxysqlv1alpha1.ProxySQLCluster, endpoints []podEndpoint) (*adminTLS, error) {
	if !cluster.Spec.TLSEnabled() {
		return nil, nil
	}

	var ss appsv1.StatefulSet
	err := c.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, &ss)
	if apierrors.IsNotFound(err) {
		return nil, nil // no pods exist, nothing will be dialed
	}
	if err != nil {
		return nil, fmt.Errorf("get statefulset for TLS dial config: %w", err)
	}
	secretName := ""
	for _, v := range ss.Spec.Template.Spec.Volumes {
		if v.Name == builders.TLSVolumeName && v.Secret != nil {
			secretName = v.Secret.SecretName
		}
	}
	if secretName == "" {
		return nil, nil // pods not TLS-wired (enable rollout hasn't rendered yet)
	}

	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cluster.Namespace}, &sec); err != nil {
		return nil, fmt.Errorf("get tls secret %q for dial config: %w", secretName, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(sec.Data["ca.crt"]) {
		return nil, fmt.Errorf("tls secret %q: ca.crt contains no usable certificates", secretName)
	}

	names := make(map[string]string, len(endpoints))
	for _, ep := range endpoints {
		names[ep.Addr] = ep.ServerName
	}
	return &adminTLS{
		base:        &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		serverNames: names,
	}, nil
}
