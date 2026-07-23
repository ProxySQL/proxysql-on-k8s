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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
	"github.com/ProxySQL/kubernetes/operator/internal/tlsutil"
)

// reasonTLSRotationError is the Degraded reason while a TLS rotation
// window is open: some ready replica has not yet been verified (by
// handshake) to serve the rotated certificate. Non-wedging; the requeue
// backoff paces the RELOAD-and-verify retries until the window expires
// into the rolling-restart fallback.
const reasonTLSRotationError = "TLSRotationError"

const (
	// annotationTLSAppliedHash is the OBJECT-level (never pod-template) STS
	// annotation recording the tlsContentHash of the tls Secret content the
	// ready replicas were last verified to SERVE. Rotation changes Secret
	// content, never cnf text, so the cnf/vars/structural machinery is
	// blind to it by design — this marker is its content-level counterpart,
	// with the same crash-safety contract: it advances only after every
	// ready replica passed handshake verification (or the restart fallback
	// was committed in the same STS write), so an operator crash mid-
	// rotation re-runs RELOAD TLS (idempotent) rather than dropping it.
	annotationTLSAppliedHash = "proxysql.com/tls-applied-hash"

	// annotationTLSRotationState is the OBJECT-level STS annotation
	// carrying the open rotation window: "<secretHash>@<RFC3339 start>".
	// PROXYSQL RELOAD TLS re-reads files the kubelet may not have
	// propagated into the Secret mount yet (the datadir symlinks resolve
	// through the mount's atomic ..data swap, which can lag the API write
	// by up to a kubelet sync period), so failed verification is retried
	// with backoff until the window — anchored at the FIRST attempt,
	// preserved across operator restarts — expires; only then does the
	// engine fall back to a rolling restart.
	annotationTLSRotationState = "proxysql.com/tls-rotation-state"

	// defaultTLSRotationWindow bounds those retries: comfortably above the
	// kubelet's default sync period (~1m worst-case Secret propagation)
	// plus reload/handshake latency.
	defaultTLSRotationWindow = 2 * time.Minute

	// tlsVerifyAttempts / defaultTLSVerifyRetryDelay bound the quick
	// IN-pass verification retries after a RELOAD (the reload itself is
	// near-synchronous; this only absorbs listener re-arm latency). The
	// cross-pass window above absorbs the long tail.
	tlsVerifyAttempts          = 3
	defaultTLSVerifyRetryDelay = 300 * time.Millisecond
)

// tlsRotationVerdict classifies the marker-vs-content comparison.
type tlsRotationVerdict int

const (
	// tlsVerdictAdopt: no marker yet — a StatefulSet from before this
	// annotation existed (operator upgrade) or the first TLS enable (where
	// the template diff already drives a rollout that mounts the current
	// content). Adopt the hash without dialing; same legacy-adoption rule
	// as the structural marker.
	tlsVerdictAdopt tlsRotationVerdict = iota
	// tlsVerdictNone: marker matches the content — nothing to do.
	tlsVerdictNone
	// tlsVerdictRotate: content moved — RELOAD TLS + verify per ready
	// replica.
	tlsVerdictRotate
)

// classifyTLSRotation is the pure decision core: the recorded marker
// against the current tls Secret content hash.
func classifyTLSRotation(applied, secretHash string) tlsRotationVerdict {
	switch applied {
	case "":
		return tlsVerdictAdopt
	case secretHash:
		return tlsVerdictNone
	default:
		return tlsVerdictRotate
	}
}

// tlsContentHash is a deterministic SHA-256 over the three Secret keys
// ProxySQL serves (the files behind the datadir symlinks, all re-read by
// PROXYSQL RELOAD TLS). Other keys a Secret may carry (cert-manager's
// tls-combined.pem etc.) never reach the pods and must not trigger
// rotations. Length-prefixed framing, same shape as structuralHash.
func tlsContentHash(data map[string][]byte) string {
	h := sha256.New()
	for _, k := range []string{"ca.crt", "tls.crt", "tls.key"} {
		v := data[k]
		_, _ = fmt.Fprintf(h, "%d:%s:%d:", len(k), k, len(v))
		_, _ = h.Write(v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// formatTLSRotationState encodes the open rotation window for
// annotationTLSRotationState.
func formatTLSRotationState(hash string, start time.Time) string {
	return hash + "@" + start.UTC().Format(time.RFC3339)
}

// parseTLSRotationState returns the recorded window start when state
// matches wantHash, or now (a fresh window) when the state is absent,
// malformed, for a superseded hash, or claims a future start (clock skew
// must not extend the window).
func parseTLSRotationState(state, wantHash string, now time.Time) time.Time {
	hash, ts, ok := strings.Cut(state, "@")
	if !ok || hash != wantHash {
		return now
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil || t.After(now) {
		return now
	}
	return t
}

// tlsRotationOutcome is what resolveTLSRotation asks the caller to commit
// in this reconcile's single StatefulSet write (the crash-safe commit
// point, exactly like the vars/structural markers).
type tlsRotationOutcome struct {
	applied string // annotationTLSAppliedHash value; "" removes it
	state   string // annotationTLSRotationState value; "" removes it
	restart string // builders.TLSRestartAnnotation pod-template value; "" omits it
	summary string // Progressing-condition summary ("" = no news)
}

// mergeSummaries folds the vars-engine and TLS-engine summaries into the
// single Progressing message: a RestartRequired from either engine must be
// surfaced (a rollout is pending and needs explaining); otherwise the vars
// summary wins as the rarer event.
func mergeSummaries(varsSummary, tlsSummary string) string {
	switch {
	case strings.HasPrefix(varsSummary, "RestartRequired"):
		return varsSummary
	case strings.HasPrefix(tlsSummary, "RestartRequired"):
		return tlsSummary
	case varsSummary != "":
		return varsSummary
	default:
		return tlsSummary
	}
}

// tlsRotationWindow returns the configured cross-pass retry window.
func (r *ProxySQLClusterReconciler) tlsRotationWindow() time.Duration {
	if r.TLSRotationWindow > 0 {
		return r.TLSRotationWindow
	}
	return defaultTLSRotationWindow
}

// resolveTLSRotation decides what the TLS rotation annotations should be
// for this reconcile and — when the tls Secret content moved — pushes
// PROXYSQL RELOAD TLS to every ready replica and verifies by handshake
// that each one now serves the new leaf.
//
// Mirrors resolveRestartChecksum's contract: pure classification first,
// dials only on the rotate verdict, and the returned outcome is committed
// by the caller's ensureStatefulSet write. A non-nil error means the
// rotation window is still open (some replica unverified): the caller
// keeps the marker unadvanced (out.applied), persists the window start
// (out.state) and requeues — the retry re-RELOADs, which is idempotent.
//
// tlsReady is ensureTLSSecrets' verdict: false (resolution failed, hold in
// effect) freezes all rotation state — the operator never rotates pods
// onto unvalidated material.
func (r *ProxySQLClusterReconciler) resolveTLSRotation(
	ctx context.Context,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	b *builders.Builder,
	tlsReady bool,
	cur stsAnnotations,
	radminPassword string,
) (tlsRotationOutcome, error) {
	keep := tlsRotationOutcome{applied: cur.tlsApplied, state: cur.tlsRotationState, restart: cur.tlsRestart}

	if !b.Spec.TLSEnabled() {
		// TLS off (or dropped by the never-wired hold): remove all rotation
		// markers. Re-enabling later starts from the adoption rule.
		return tlsRotationOutcome{}, nil
	}
	if !tlsReady {
		return keep, nil
	}

	secretName := b.TLSMountSecret
	if secretName == "" {
		secretName = b.TLSSecretName()
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: b.Namespace()}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted between validation and here; the next reconcile's
			// resolution degrades properly.
			return keep, nil
		}
		return keep, fmt.Errorf("get tls secret %q for rotation: %w", secretName, err)
	}
	secretHash := tlsContentHash(sec.Data)

	switch classifyTLSRotation(cur.tlsApplied, secretHash) {
	case tlsVerdictAdopt, tlsVerdictNone:
		// Either way the recorded state converges on the current content;
		// any stale window state is cleared.
		return tlsRotationOutcome{applied: secretHash, restart: cur.tlsRestart}, nil
	}

	// tlsVerdictRotate: the Secret content moved under running pods.
	adminPort := b.Spec.Protocols.Admin.Port
	endpoints, err := discoverPodEndpoints(ctx, r.Client, cluster, adminPort)
	if err != nil {
		return keep, err
	}
	if len(endpoints) == 0 {
		// Keep the marker UNADVANCED: a pod that is merely NotReady right
		// now may still serve the old cert once it recovers, and freshly
		// booting pods trigger a new reconcile (STS status change) that
		// verifies them here. Any open window state is preserved: pods
		// flapping Ready/NotReady mid-rotation must not reset the clock
		// (that would defer the restart fallback indefinitely), and stale
		// state is hash- and skew-guarded by parseTLSRotationState.
		return tlsRotationOutcome{applied: cur.tlsApplied, state: cur.tlsRotationState, restart: cur.tlsRestart}, nil
	}

	expectedFP, err := tlsutil.LeafFingerprint(sec.Data["tls.crt"])
	if err != nil {
		return keep, fmt.Errorf("tls secret %q: parsing tls.crt: %w", secretName, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(sec.Data["ca.crt"]) {
		return keep, fmt.Errorf("tls secret %q: ca.crt contains no usable certificates", secretName)
	}
	start := parseTLSRotationState(cur.tlsRotationState, secretHash, time.Now())
	var failures []string
	for _, ep := range endpoints {
		dialCfg := pinnedTLSConfig(pool, expectedFP, ep.ServerName)
		if verr := r.reloadAndVerifyTLS(ctx, ep.Addr, radminPassword, expectedFP, dialCfg); verr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ep.Addr, verr))
		}
	}

	if len(failures) == 0 {
		return tlsRotationOutcome{
			applied: secretHash,
			restart: cur.tlsRestart,
			summary: fmt.Sprintf("RuntimeApplied: TLS certificate reloaded on %d replica(s)", len(endpoints)),
		}, nil
	}

	if time.Since(start) >= r.tlsRotationWindow() {
		// Bounded window exhausted: rolling-restart fallback. Bumping the
		// pod-template annotation to the CONTENT hash makes the restart
		// idempotent (one rollout per rotated content, crash-safe); the
		// marker advances in the same STS write because the rollout itself
		// now delivers the new material (verdictStructural's contract).
		return tlsRotationOutcome{
			applied: secretHash,
			restart: secretHash,
			summary: "RestartRequired: TLS rotation fallback (" + joinTrunc(failures, 256) + ")",
		}, nil
	}

	return tlsRotationOutcome{
			applied: cur.tlsApplied,
			state:   formatTLSRotationState(secretHash, start),
			restart: cur.tlsRestart,
		}, fmt.Errorf("TLS rotation pending on %d/%d replica(s): %s",
			len(failures), len(endpoints), joinTrunc(failures, 512))
}

// reloadAndVerifyTLS drives one replica through reload-then-verify:
// skip the reload when the pod already serves the expected leaf (a peer's
// earlier pass, or a pod that booted straight onto the new content), else
// PROXYSQL RELOAD TLS and re-probe with quick bounded retries. dialCfg is
// the pinnedTLSConfig for the rotation's target content.
func (r *ProxySQLClusterReconciler) reloadAndVerifyTLS(ctx context.Context, addr, radminPassword, expectedFP string, dialCfg *tls.Config) error {
	probe := r.tlsProbe
	if probe == nil {
		probe = probeTLSLeafFingerprint
	}
	reload := r.tlsReload
	if reload == nil {
		reload = reloadTLSOn
	}
	delay := r.tlsVerifyRetryDelay
	if delay <= 0 {
		delay = defaultTLSVerifyRetryDelay
	}

	if fp, err := probe(ctx, addr, radminUser, radminPassword, dialCfg); err == nil && fp == expectedFP {
		return nil
	}
	if err := reload(ctx, addr, radminUser, radminPassword, dialCfg); err != nil {
		return fmt.Errorf("reload tls: %w", err)
	}

	var lastErr error
	for attempt := range tlsVerifyAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		fp, err := probe(ctx, addr, radminUser, radminPassword, dialCfg)
		if err != nil {
			lastErr = fmt.Errorf("verify handshake: %w", err)
			continue
		}
		if fp == expectedFP {
			return nil
		}
		lastErr = fmt.Errorf("still serving certificate %.12s… (want %.12s…)", fp, expectedFP)
	}
	return lastErr
}

// pinnedTLSConfig returns the rotation dials' tls.Config for ONE endpoint:
// the peer is accepted iff its leaf is EXACTLY the Secret's new tls.crt
// (fingerprint pin) or its chain verifies against the Secret's current CA
// pool AND covers serverName — the endpoint's per-pod DNS identity (the
// not-yet-reloaded old cert in every tier that keeps the CA stable, whose
// wildcard SAN covers the pod). Chain trust alone is not identity: with a
// shared corporate CA or ClusterIssuer, any cert+key from the same CA
// would otherwise pass and receive the radmin password. The fingerprint
// branch stays name-free: mid-rotation the pinned content IS the identity,
// so a user cert lacking the per-pod SAN degrades to the restart fallback
// rather than blocking the rotation.
//
// InsecureSkipVerify here disables only the DEFAULT verifier so that
// VerifyPeerCertificate below is the sole authority — this is Go's
// documented pattern for custom verification, not "no verification": a
// peer presenting anything outside those two identities fails the
// handshake before any credential is sent. The deliberately-unverifiable
// corner — a tier-1 rotation replacing leaf AND CA in one write, probed
// while the pod still serves the old material — therefore cannot be
// reloaded over a verified channel; the engine's bounded window then falls
// back to the rolling restart, where the kubelet (not the network)
// delivers the new material. ServerName is set for SNI only (the default
// verifier is off); the chained branch's DNSName check below is what
// enforces it.
func pinnedTLSConfig(pool *x509.CertPool, expectedFP, serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: true, // custom pinned verification below is the sole verifier; see doc comment
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("no peer certificate presented")
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) == expectedFP {
				return nil
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parsing peer certificate: %w", err)
			}
			intermediates := x509.NewCertPool()
			for _, raw := range rawCerts[1:] {
				if ic, perr := x509.ParseCertificate(raw); perr == nil {
					intermediates.AddCert(ic)
				}
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, DNSName: serverName}); err != nil {
				return fmt.Errorf("peer certificate is neither the expected rotated leaf nor chained to the cluster CA for %q: %w", serverName, err)
			}
			return nil
		},
	}
}

// probeTLSLeafFingerprint completes a TLS handshake on the ADMIN port
// (MySQL-wire STARTTLS, handled by the driver) and returns the SHA-256
// fingerprint of the served leaf certificate. The admin port serves the
// same datadir certificates as the data ports (gate-verified on
// proxysql:3.0), and unlike the data ports it can never be
// protocol-disabled.
//
// The fingerprint is CAPTURED before cfg's pinned verifier runs, so the
// probe can still OBSERVE and report a stale leaf the verifier rejects
// (tier-1 full-bundle swap) — the handshake aborts in that case, so no
// credential ever reaches the unverified peer; only the observation
// escapes. The TLS handshake happens before MySQL authentication, so a
// Ping failing at auth still proves which certificate is served — the
// fingerprint is only discarded when the handshake itself never completed.
func probeTLSLeafFingerprint(ctx context.Context, addr, user, pass string, cfg *tls.Config) (string, error) {
	var fp string
	probeCfg := cfg.Clone()
	verify := probeCfg.VerifyPeerCertificate
	probeCfg.VerifyPeerCertificate = func(rawCerts [][]byte, chains [][]*x509.Certificate) error {
		if len(rawCerts) > 0 {
			sum := sha256.Sum256(rawCerts[0])
			fp = hex.EncodeToString(sum[:])
		}
		if verify != nil {
			return verify(rawCerts, chains)
		}
		return nil
	}
	c, err := proxysqlclient.NewWithTLS(addr, user, pass, probeCfg)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	if err := c.Ping(ctx); err != nil && fp == "" {
		return "", err
	}
	return fp, nil
}

// reloadTLSOn dials the admin port with the pinned config and issues
// PROXYSQL RELOAD TLS. Requiring the NEW cert alone to deliver the command
// that installs the new cert would be circular, hence the pinned config's
// or-chained-to-current-CA acceptance; a peer satisfying neither (see
// pinnedTLSConfig) fails the handshake before the radmin password is sent,
// and the rotation falls back to the kubelet-delivered rolling restart.
func reloadTLSOn(ctx context.Context, addr, user, pass string, cfg *tls.Config) error {
	c, err := proxysqlclient.NewWithTLS(addr, user, pass, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return proxysqlclient.ReloadTLS(ctx, c)
}

// resolveTLSCleanup decides Builder.TLSCleanup: when spec.tls is now
// absent/disabled but the LIVE StatefulSet template shows TLS wiring (the
// tls-init container) or a previous cleanup, keep rendering the
// tls-cleanup init container so persistent datadirs shed their (dangling)
// cert symlinks — probe-verified to otherwise crash proxysql at boot (see
// builders.Builder.TLSCleanup). It stays rendered from then on: the
// container is an idempotent no-op guard, and dropping it later would
// churn the template for no benefit.
func (r *ProxySQLClusterReconciler) resolveTLSCleanup(ctx context.Context, b *builders.Builder) error {
	if b.Spec.TLSEnabled() {
		return nil
	}
	var ss appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Name: b.Name(), Namespace: b.Namespace()}, &ss)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get statefulset for TLS cleanup: %w", err)
	}
	for _, c := range ss.Spec.Template.Spec.InitContainers {
		if c.Name == builders.TLSInitContainerName || c.Name == builders.TLSCleanupInitContainerName {
			b.TLSCleanup = true
			return nil
		}
	}
	return nil
}
