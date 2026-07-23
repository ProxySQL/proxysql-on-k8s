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
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/tlsutil"
)

// ---- pure helpers ----

func TestTLSContentHash(t *testing.T) {
	base := map[string][]byte{
		"tls.crt": []byte("CERT"),
		"tls.key": []byte("KEY"),
		"ca.crt":  []byte("CA"),
	}
	h := tlsContentHash(base)
	if h == "" || len(h) != 64 {
		t.Fatalf("hash %q, want 64 hex chars", h)
	}
	if h != tlsContentHash(base) {
		t.Errorf("hash not deterministic")
	}

	// Any of the three served keys moving must move the hash — RELOAD TLS
	// re-reads all three files.
	for _, k := range []string{"tls.crt", "tls.key", "ca.crt"} {
		mut := map[string][]byte{}
		maps.Copy(mut, base)
		mut[k] = append([]byte("x-"), mut[k]...)
		if tlsContentHash(mut) == h {
			t.Errorf("changing %q did not change the hash", k)
		}
	}

	// Unserved keys (e.g. cert-manager's tls-combined.pem) must NOT move
	// the hash: only content ProxySQL serves triggers rotation.
	withExtra := map[string][]byte{}
	maps.Copy(withExtra, base)
	withExtra["tls-combined.pem"] = []byte("COMBINED")
	if tlsContentHash(withExtra) != h {
		t.Errorf("unserved Secret key changed the hash")
	}

	// A value moving between keys must not collide (framing).
	swapped := map[string][]byte{
		"tls.crt": []byte("CA"),
		"tls.key": []byte("KEY"),
		"ca.crt":  []byte("CERT"),
	}
	if tlsContentHash(swapped) == h {
		t.Errorf("key/value swap collided")
	}
}

func TestClassifyTLSRotation(t *testing.T) {
	tests := []struct {
		applied, hash string
		want          tlsRotationVerdict
	}{
		// Empty marker: legacy StatefulSet (operator upgrade) or first
		// enable — adopt without dialing, exactly like the structural
		// marker's legacy-adoption rule.
		{"", "h1", tlsVerdictAdopt},
		{"h1", "h1", tlsVerdictNone},
		{"h1", "h2", tlsVerdictRotate},
	}
	for _, tt := range tests {
		if got := classifyTLSRotation(tt.applied, tt.hash); got != tt.want {
			t.Errorf("classifyTLSRotation(%q, %q) = %v, want %v", tt.applied, tt.hash, got, tt.want)
		}
	}
}

func TestTLSRotationState(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	start := now.Add(-40 * time.Second)

	state := formatTLSRotationState("h1", start)

	// Round-trip: same hash → the recorded start survives (crash window:
	// an operator restart mid-rotation must not restart the clock).
	if got := parseTLSRotationState(state, "h1", now); !got.Equal(start) {
		t.Errorf("round-trip start = %v, want %v", got, start)
	}
	// A DIFFERENT secret hash means a new rotation superseded the old one
	// mid-window: the clock restarts.
	if got := parseTLSRotationState(state, "h2", now); !got.Equal(now) {
		t.Errorf("stale-hash start = %v, want now", got)
	}
	// Garbage or absent state restarts the clock rather than erroring.
	for _, s := range []string{"", "garbage", "h1@not-a-time"} {
		if got := parseTLSRotationState(s, "h1", now); !got.Equal(now) {
			t.Errorf("parse(%q) = %v, want now", s, got)
		}
	}
	// A future timestamp (clock skew, tampering) must not extend the
	// window indefinitely.
	future := formatTLSRotationState("h1", now.Add(time.Hour))
	if got := parseTLSRotationState(future, "h1", now); !got.Equal(now) {
		t.Errorf("future start = %v, want now", got)
	}
}

func TestPodDNSName(t *testing.T) {
	// Must be covered by the wildcard SAN *.<name>-headless.<ns>.svc
	// (one label: the pod name).
	got := podDNSName("pxc-0", "pxc", "prod")
	if got != "pxc-0.pxc-headless.prod.svc" {
		t.Errorf("podDNSName = %q", got)
	}
}

func TestMergeSummaries(t *testing.T) {
	tests := []struct{ vars, tls, want string }{
		{"", "", ""},
		{"RuntimeApplied: mysql-max_connections", "", "RuntimeApplied: mysql-max_connections"},
		{"", "RuntimeApplied: TLS certificate reloaded on 2 replica(s)", "RuntimeApplied: TLS certificate reloaded on 2 replica(s)"},
		// A pending rollout must be explained no matter which engine
		// triggered it.
		{"RuntimeApplied: x", "RestartRequired: TLS rotation fallback", "RestartRequired: TLS rotation fallback"},
		{"RestartRequired: structural cnf change", "RuntimeApplied: TLS certificate reloaded on 1 replica(s)", "RestartRequired: structural cnf change"},
		{"RuntimeApplied: x", "RuntimeApplied: y", "RuntimeApplied: x"},
	}
	for _, tt := range tests {
		if got := mergeSummaries(tt.vars, tt.tls); got != tt.want {
			t.Errorf("mergeSummaries(%q, %q) = %q, want %q", tt.vars, tt.tls, got, tt.want)
		}
	}
}

// ---- engine (fake client + injected probe/reload) ----

// rotationFixture assembles a reconciler with a fake client around a
// TLS-enabled cluster, its mounted serving-cert Secret (real tlsutil
// material so fingerprints are honest), and optionally ready pods.
type rotationFixture struct {
	r       *ProxySQLClusterReconciler
	cluster *proxysqlv1alpha1.ProxySQLCluster
	b       *builders.Builder
	hash    string // tlsContentHash of the mounted Secret
	leafFP  string // fingerprint of the Secret's tls.crt
	probes  []string
	reloads []string
	// probeFP is what the fake pod at addr currently "serves";
	// reloadedAddrs tracks which pods saw a RELOAD TLS (per address — a
	// reload on one replica must not affect its peers).
	probeFP       func(addr string) string
	reloadedAddrs map[string]bool
}

func newRotationFixture(t *testing.T, readyPods int) *rotationFixture {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := proxysqlv1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	cluster := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "rot", Namespace: "default"},
		Spec: proxysqlv1alpha1.ProxySQLClusterSpec{
			TLS: &proxysqlv1alpha1.TLSSpec{Enabled: true},
		},
	}

	caCrt, caKey, err := tlsutil.NewCA("test-ca", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	crt, key, err := tlsutil.IssueServing(caCrt, caKey, tlsutil.SANsFor("rot", "default", nil), time.Hour)
	if err != nil {
		t.Fatalf("IssueServing: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rot-tls", Namespace: "default"},
		Data:       map[string][]byte{"tls.crt": crt, "tls.key": key, "ca.crt": caCrt},
	}
	leafFP, err := tlsutil.LeafFingerprint(crt)
	if err != nil {
		t.Fatalf("LeafFingerprint: %v", err)
	}

	objs := make([]runtime.Object, 0, 2+readyPods)
	objs = append(objs, cluster, sec)
	for i := range readyPods {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("rot-%d", i),
				Namespace: "default",
				Labels:    map[string]string{"proxysql.com/cluster": "rot"},
			},
			Status: corev1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.0.%d", i+1),
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		})
	}

	fx := &rotationFixture{cluster: cluster, hash: tlsContentHash(sec.Data), leafFP: leafFP, reloadedAddrs: map[string]bool{}}
	fx.probeFP = func(string) string { return "stale-fingerprint" }
	fx.r = &ProxySQLClusterReconciler{
		Client:              fake.NewClientBuilder().WithScheme(sch).WithRuntimeObjects(objs...).Build(),
		Scheme:              sch,
		tlsVerifyRetryDelay: time.Millisecond,
		tlsProbe: func(_ context.Context, addr, _, _ string, _ *tls.Config) (string, error) {
			fx.probes = append(fx.probes, addr)
			return fx.probeFP(addr), nil
		},
		tlsReload: func(_ context.Context, addr, _, _ string, _ *tls.Config) error {
			fx.reloads = append(fx.reloads, addr)
			fx.reloadedAddrs[addr] = true
			return nil
		},
	}
	fx.b = builders.New(cluster, sch, builders.Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	fx.b.TLSMountSecret = "rot-tls"
	return fx
}

func (fx *rotationFixture) resolve(t *testing.T, cur stsAnnotations) (tlsRotationOutcome, error) {
	t.Helper()
	return fx.r.resolveTLSRotation(context.Background(), fx.cluster, fx.b, true, cur, "pw")
}

func TestResolveTLSRotation_DisabledClearsMarkers(t *testing.T) {
	fx := newRotationFixture(t, 0)
	fx.b.Spec.TLS = nil // post-hold builder state: no TLS rendering this pass

	out, err := fx.r.resolveTLSRotation(context.Background(), fx.cluster, fx.b, false,
		stsAnnotations{tlsApplied: "old", tlsRotationState: "old@2026-01-01T00:00:00Z", tlsRestart: "old"}, "pw")
	if err != nil {
		t.Fatalf("resolveTLSRotation: %v", err)
	}
	if out.applied != "" || out.state != "" || out.restart != "" {
		t.Errorf("outcome = %+v, want all-empty (markers removed when TLS is off)", out)
	}
	if len(fx.probes)+len(fx.reloads) != 0 {
		t.Errorf("dialed while disabled: probes=%v reloads=%v", fx.probes, fx.reloads)
	}
}

func TestResolveTLSRotation_NotReadyHoldsEverything(t *testing.T) {
	fx := newRotationFixture(t, 1)
	cur := stsAnnotations{tlsApplied: "old", tlsRotationState: "s", tlsRestart: "rr"}

	out, err := fx.r.resolveTLSRotation(context.Background(), fx.cluster, fx.b, false, cur, "pw")
	if err != nil {
		t.Fatalf("resolveTLSRotation: %v", err)
	}
	if out.applied != "old" || out.state != "s" || out.restart != "rr" {
		t.Errorf("outcome = %+v, want held values (never rotate onto unvalidated material)", out)
	}
	if len(fx.probes)+len(fx.reloads) != 0 {
		t.Errorf("dialed while degraded: probes=%v reloads=%v", fx.probes, fx.reloads)
	}
}

func TestResolveTLSRotation_AdoptsWhenMarkerEmpty(t *testing.T) {
	fx := newRotationFixture(t, 2)

	out, err := fx.resolve(t, stsAnnotations{tlsApplied: "", tlsRestart: "keep"})
	if err != nil {
		t.Fatalf("resolveTLSRotation: %v", err)
	}
	if out.applied != fx.hash {
		t.Errorf("applied = %q, want secret hash %q (legacy adoption)", out.applied, fx.hash)
	}
	if out.restart != "keep" {
		t.Errorf("restart = %q, want carried value", out.restart)
	}
	if out.state != "" || out.summary != "" {
		t.Errorf("outcome = %+v, want silent adoption", out)
	}
	if len(fx.probes)+len(fx.reloads) != 0 {
		t.Errorf("adoption must not dial: probes=%v reloads=%v", fx.probes, fx.reloads)
	}
}

func TestResolveTLSRotation_NoOpClearsStaleState(t *testing.T) {
	fx := newRotationFixture(t, 2)

	out, err := fx.resolve(t, stsAnnotations{
		tlsApplied:       fx.hash,
		tlsRotationState: formatTLSRotationState(fx.hash, time.Now()),
	})
	if err != nil {
		t.Fatalf("resolveTLSRotation: %v", err)
	}
	if out.applied != fx.hash || out.state != "" {
		t.Errorf("outcome = %+v, want marker kept and stale window state cleared", out)
	}
	if len(fx.probes)+len(fx.reloads) != 0 {
		t.Errorf("no-op must not dial: probes=%v reloads=%v", fx.probes, fx.reloads)
	}
}

func TestResolveTLSRotation_ZeroReadyPodsKeepsMarkerUnadvanced(t *testing.T) {
	fx := newRotationFixture(t, 0)

	out, err := fx.resolve(t, stsAnnotations{tlsApplied: "old-hash"})
	if err != nil {
		t.Fatalf("resolveTLSRotation: %v", err)
	}
	if out.applied != "old-hash" {
		t.Errorf("applied = %q, want unadvanced old-hash (a NotReady pod may still serve the old cert)", out.applied)
	}
	if out.state != "" {
		t.Errorf("state = %q, want empty (no rotation window without dials)", out.state)
	}
}

func TestResolveTLSRotation_AlreadyServingSkipsReload(t *testing.T) {
	fx := newRotationFixture(t, 2)
	fx.probeFP = func(string) string { return fx.leafFP }

	out, err := fx.resolve(t, stsAnnotations{tlsApplied: "old-hash"})
	if err != nil {
		t.Fatalf("resolveTLSRotation: %v", err)
	}
	if out.applied != fx.hash {
		t.Errorf("applied = %q, want %q", out.applied, fx.hash)
	}
	if len(fx.reloads) != 0 {
		t.Errorf("reloads = %v, want none (pre-probe already saw the new leaf)", fx.reloads)
	}
	if !strings.HasPrefix(out.summary, "RuntimeApplied") {
		t.Errorf("summary = %q, want RuntimeApplied prefix", out.summary)
	}
}

func TestResolveTLSRotation_ReloadThenVerifiedAdvancesMarker(t *testing.T) {
	fx := newRotationFixture(t, 2)
	// Each pod serves the old leaf until ITS OWN RELOAD TLS lands.
	fx.probeFP = func(addr string) string {
		if fx.reloadedAddrs[addr] {
			return fx.leafFP
		}
		return "stale-fingerprint"
	}

	out, err := fx.resolve(t, stsAnnotations{tlsApplied: "old-hash"})
	if err != nil {
		t.Fatalf("resolveTLSRotation: %v", err)
	}
	if out.applied != fx.hash {
		t.Errorf("applied = %q, want advanced to %q", out.applied, fx.hash)
	}
	if out.state != "" || out.restart != "" {
		t.Errorf("outcome = %+v, want no window state and no restart bump", out)
	}
	if len(fx.reloads) != 2 {
		t.Errorf("reloads = %v, want one per ready replica", fx.reloads)
	}
	if !strings.HasPrefix(out.summary, "RuntimeApplied") {
		t.Errorf("summary = %q, want RuntimeApplied prefix", out.summary)
	}
}

func TestResolveTLSRotation_FailurePersistsWindowStart(t *testing.T) {
	fx := newRotationFixture(t, 1)
	// probeFP stays stale: verification never succeeds.

	out, err := fx.resolve(t, stsAnnotations{tlsApplied: "old-hash"})
	if err == nil {
		t.Fatalf("want error while the rotation window is open (drives requeue/backoff)")
	}
	if out.applied != "old-hash" {
		t.Errorf("applied = %q, want unadvanced (retry must re-RELOAD)", out.applied)
	}
	if out.restart != "" {
		t.Errorf("restart = %q, want no fallback before the window expires", out.restart)
	}
	if !strings.HasPrefix(out.state, fx.hash+"@") {
		t.Fatalf("state = %q, want a persisted window start for hash %q", out.state, fx.hash)
	}

	// Crash window: a second pass (operator restarted, same annotations
	// fed back) must keep the ORIGINAL window start, not restart the clock.
	out2, err2 := fx.resolve(t, stsAnnotations{tlsApplied: "old-hash", tlsRotationState: out.state})
	if err2 == nil {
		t.Fatalf("want error on second failing pass")
	}
	if out2.state != out.state {
		t.Errorf("state moved %q → %q; window start must survive operator restarts", out.state, out2.state)
	}
}

func TestResolveTLSRotation_WindowExpiredFallsBackToRestart(t *testing.T) {
	fx := newRotationFixture(t, 2)
	// A window that started long ago and has expired.
	state := formatTLSRotationState(fx.hash, time.Now().Add(-time.Hour))

	out, err := fx.resolve(t, stsAnnotations{tlsApplied: "old-hash", tlsRotationState: state})
	if err != nil {
		t.Fatalf("fallback is a decision, not an error: %v", err)
	}
	if out.restart != fx.hash {
		t.Errorf("restart = %q, want the secret hash (idempotent one-restart-per-content bump)", out.restart)
	}
	if out.applied != fx.hash {
		t.Errorf("applied = %q, want advanced (the rollout delivers the new material)", out.applied)
	}
	if out.state != "" {
		t.Errorf("state = %q, want cleared after fallback", out.state)
	}
	if !strings.HasPrefix(out.summary, "RestartRequired") {
		t.Errorf("summary = %q, want RestartRequired prefix", out.summary)
	}
}
