# TLS Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `spec.tls` on `ProxySQLCluster`: three-tier cert issuance (user Secret > cert-manager > operator self-signed), TLS on frontend/backend/admin+cluster surfaces, and restart-free rotation via `PROXYSQL RELOAD TLS` verified by handshake with rolling-restart fallback.

**Architecture:** A pure cert toolkit (`internal/tlsutil`) generates/parses certs; the cluster reconciler resolves the tier into two Secrets (`<cluster>-tls`, `<cluster>-tls-ca`), mounts one, renders TLS variables into the cnf (reserved, structural), and drives rotation off a `tls-applied-hash` STS marker with handshake verification. `proxysqlclient` gains TLS dialing used by both reconcilers. cert-manager is integrated via unstructured objects (the ServiceMonitor precedent) — no new Go dependency.

**Tech Stack:** Go crypto/x509 + crypto/tls, go-sql-driver `RegisterTLSConfig`, controller-runtime unstructured for cert-manager, kind e2e.

**Spec:** `docs/superpowers/specs/2026-07-23-tls-management-design.md` — governs on conflict.

## Global Constraints

- Branch `feat/tls` off `main`. Go from `operator/` with `GOTOOLCHAIN=go1.25.10`; `make sync-crds` from root; never hand-edit chart CRDs.
- `spec.tls` absent/false ⇒ **byte-identical rendering** — `TestGolden` must pass unchanged in every task.
- Builders pure; no new module dependencies (cert-manager via unstructured, like `ensureServiceMonitor`).
- All rendered TLS variables join `reservedCnfKeys` (not overridable via `spec.variables`) and are structural for classification (their VALUES are file paths that never change at rotation; content rotation must NOT look like a cnf change).
- Degraded reasons: `TLSSecretError` (resolution failures), non-wedging like `ExternalServiceError` (set condition, continue reconcile, requeue).
- **Exact ProxySQL 3.0 variable names must be verified against the shipped `proxysql/proxysql:3.0` image before use** (admin `SELECT * FROM global_variables WHERE variable_name LIKE '%ssl%'` in a throwaway container is definitive). The names used in this plan are placeholders to be pinned in Task 3 and proven live by the e2e. Confirmed by the maintainer: frontend cert paths ARE configurable in 3.0.
- PSA restricted: Secret mounts read-only; no datadir writes for certs.

---

### Task 1: API — `spec.tls` + `pgsqlServers.useSSL`

**Files:**
- Modify: `operator/api/v1alpha1/proxysqlcluster_types.go` (add `TLSSpec`, `TLSIssuerRef`, `TLSBackendSpec`; field `TLS *TLSSpec` on the spec — pointer so absent stays absent)
- Modify: `operator/api/v1alpha1/proxysqlconfig_types.go` (add `UseSSL *bool` to the `PostgreSQLServer` struct, mirroring `MySQLServer.UseSSL` — read the mysql struct first and copy its exact marker/json style)
- Generated: CRD bases + chart copies + deepcopy

**Interfaces (produced, consumed by all later tasks):**

```go
type TLSSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// +optional
	IssuerRef *TLSIssuerRef `json:"issuerRef,omitempty"`
	// +optional
	// +kubebuilder:default="2160h"
	Duration metav1.Duration `json:"duration,omitempty"`
	// +optional
	// +kubebuilder:default="720h"
	RenewBefore metav1.Duration `json:"renewBefore,omitempty"`
	// +optional
	ExtraSANs []string `json:"extraSANs,omitempty"`
	// +optional
	Backend *TLSBackendSpec `json:"backend,omitempty"`
}

type TLSIssuerRef struct {
	Name string `json:"name"`
	// +optional
	// +kubebuilder:default=Issuer
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	Kind string `json:"kind,omitempty"`
	// +optional
	// +kubebuilder:default=cert-manager.io
	Group string `json:"group,omitempty"`
}

type TLSBackendSpec struct {
	// +optional
	CASecretName string `json:"caSecretName,omitempty"`
	// +optional
	ClientCertSecretName string `json:"clientCertSecretName,omitempty"`
}
```

Godoc per field per the spec (tier precedence on TLSSpec; the backend-PKI-is-different warning on TLSBackendSpec). Also helper methods on the spec type: `func (s *ProxySQLClusterSpec) TLSEnabled() bool { return s.TLS != nil && s.TLS.Enabled }`.

- [ ] **Step 1:** Add types + field + helpers; `make generate manifests` (operator/), `make sync-crds` (root).
- [ ] **Step 2:** `GOTOOLCHAIN=go1.25.10 go build ./... && go test ./...` — PASS incl. `TestGolden` (API-only).
- [ ] **Step 3:** Commit `feat(api): spec.tls (three-tier issuance, backend PKI, SANs) + pgsqlServers.useSSL`.

---

### Task 2: `internal/tlsutil` — pure cert toolkit (TDD)

**Files:**
- Create: `operator/internal/tlsutil/tlsutil.go`, `operator/internal/tlsutil/tlsutil_test.go`

**Interfaces (produced):**

```go
// NewCA returns a self-signed CA (PEM cert + key) valid for duration.
func NewCA(commonName string, duration time.Duration) (certPEM, keyPEM []byte, err error)

// IssueServing signs a serving cert for the DNS names/IPs with the CA.
func IssueServing(caCertPEM, caKeyPEM []byte, sans []string, duration time.Duration) (certPEM, keyPEM []byte, err error)

// NeedsRenewal reports whether certPEM expires within renewBefore
// (or fails to parse — parse failure counts as needing renewal).
func NeedsRenewal(certPEM []byte, renewBefore time.Duration) bool

// LeafFingerprint returns the SHA-256 fingerprint of the first PEM cert.
func LeafFingerprint(certPEM []byte) (string, error)

// SANsFor derives the full SAN set for a cluster: name, name.ns,
// name.ns.svc, *.<name>-headless.<ns>.svc, <name>-external, plus extras.
// Entries parsing as IPs become IP SANs.
func SANsFor(name, namespace string, extras []string) []string
```

- [ ] **Step 1:** Failing tests: CA round-trip (issue serving cert, verify chain with x509.Verify against the CA pool); SAN set exact-match incl. wildcard + IP extra; NeedsRenewal boundary (expires in renewBefore-1h → true; renewBefore+1h → false; garbage PEM → true); fingerprint stability + mismatch.
- [ ] **Step 2:** RED → implement (ECDSA P-256 keys; serial from crypto/rand; KeyUsage/ExtKeyUsage server auth; CA basic constraints) → GREEN. No clock injection needed beyond `time.Now` at call time — pass `duration`, assert with generous margins (no Date.now determinism issue in Go tests).
- [ ] **Step 3:** Full suite; commit `feat(tlsutil): self-signed CA + serving cert toolkit`.

---

### Task 3: cnf + StatefulSet wiring (TDD)

**Files:**
- Modify: `operator/internal/controller/builders/proxysql_cnf.go` (TLS variable rendering + reservedCnfKeys extension), `operator/internal/controller/builders/statefulset.go` (Secret volume/mount when TLS enabled), `operator/internal/controller/builders/builders.go` (TLS Secret name helpers: `TLSSecretName() = name + "-tls"`, `TLSCASecretName() = name + "-tls-ca"`)
- Test: `operator/internal/controller/builders/builders_test.go` (+ golden untouched)

**Interfaces:** consumes `Spec.TLSEnabled()`; produces cnf rendering + mount at `/etc/proxysql/tls` (items tls.crt/tls.key/ca.crt) used by Task 5's verification and Task 6's e2e.

- [ ] **Step 1 (pin the real variable names FIRST):** run `docker run --rm proxysql/proxysql:3.0 sh -c "proxysql --version"` and query a scratch instance's `global_variables LIKE '%ssl%'` (or read the image's bundled docs) to record the exact 3.0 names for: frontend cert/key/CA paths (mysql + pgsql), frontend have_ssl flags, admin-interface TLS enable + cert paths, cluster-sync SSL flag, backend `*-ssl_p2s_ca/cert/key`. Write them into a `tlsCnfVars` table in the code with a comment citing how they were verified. If any surface's paths are NOT configurable, STOP and report BLOCKED (the spec's architecture depends on it).
- [ ] **Step 2:** Failing builder tests: TLS disabled → cnf byte-identical (compare against a render from a spec without TLS — this is the golden guarantee at unit level); enabled → all frontend/admin/cluster variables present pointing under `/etc/proxysql/tls/`; backend variables only when `Backend.CASecretName` set; every TLS variable rejected by `validateCnfVars` when user-supplied via `spec.variables` (reservedCnfKeys extension test); STS gains the read-only Secret volume+mount only when enabled.
- [ ] **Step 3:** RED → implement → GREEN. Full suite incl. `TestGolden` (must not change — the golden spec has no TLS).
- [ ] **Step 4:** Commit `feat(builders): TLS cnf variables + Secret mount (reserved, structural)`.

---

### Task 4: Secret resolution engine + cert-manager (envtest)

**Files:**
- Create: `operator/internal/controller/tls_secrets.go` (reconciler-side: `ensureTLSSecrets`)
- Modify: `operator/internal/controller/proxysqlcluster_controller.go` (wire before cnf build — the cnf/mount need the Secret to exist; on resolution error set `Degraded=TLSSecretError` non-wedging and render WITHOUT TLS? No — render with TLS paths but the mount will fail pod start; instead: on error, keep last-good Secrets if present, else skip the TLS render for this pass and degrade. Encode exactly that rule.)
- Test: envtest in `proxysqlcluster_controller_test.go`

**Behavior (produces `ensureTLSSecrets(ctx, cluster, b) (ready bool, err error)`):**
- Tier 1 `secretName`: validate referenced Secret has tls.crt/tls.key (+ca.crt warn-if-absent → still ready, CA-less verify documented); copy/point: mount THE referenced Secret directly (no copying — mount name comes from resolution, add to builder input).
- Tier 2 `issuerRef`: ensure an unstructured `cert-manager.io/v1 Certificate` named `<cluster>-tls` (secretName `<cluster>-tls`, dnsNames from `tlsutil.SANsFor`, duration/renewBefore from spec) — mirror `ensureServiceMonitor`'s CRD-absent handling: missing cert-manager CRDs ⇒ `TLSSecretError` degraded, not a crash. Wait for the Secret to appear (ready=false until then).
- Tier 3: mint CA into `<cluster>-tls-ca` if absent (preserve existing); issue/renew serving cert into `<cluster>-tls` when absent or `tlsutil.NeedsRenewal`.
- envtest specs: tier precedence (user Secret referenced → no Certificate object created); self-signed CA preserved across reconciles (UID + data stable); renewal reissues (seed a short-duration cert, assert data changes); missing user Secret → Degraded TLSSecretError, replicas change still applies (non-wedging, copy the ExternalServiceError test shape); cert-manager tier creates the unstructured Certificate with correct SANs (CRD installed into envtest via a minimal unstructured CRD fixture — if envtest rejects that approach, cover tier 2 with a unit-level fake client test and note it).
- [ ] Steps: envtest specs first (red) → implement → full suite green → commit `feat(operator): three-tier TLS secret resolution + cert-manager integration`.

---

### Task 5: TLS dialing + rotation engine (TDD)

**Files:**
- Modify: `operator/internal/proxysqlclient/client.go` (`NewWithTLS(addr, user, pass string, tlsCfg *tls.Config) (*Client, error)`; `New` delegates with nil; per-address `RegisterTLSConfig` key derivation)
- Create: `operator/internal/controller/tls_rotation.go` (marker `proxysql.com/tls-applied-hash`; `resolveTLSRotation(...)` mirroring `resolveRestartChecksum`'s shape: content hash of the served Secret vs marker; per-ready-replica `PROXYSQL RELOAD TLS` via Exec; verify via `crypto/tls` dial to the admin port comparing `tlsutil.LeafFingerprint`; bounded retries; fallback = dedicated pod-template annotation `proxysql.com/tls-restart` bump; Degraded/requeue semantics copied from the runtime-apply engine)
- Modify: both reconcilers' dial sites (`restart_checksum.go:356`, `proxysqlconfig_controller.go:491,515` + `GetProxyConnection`-equivalents) to pass the cluster CA `tls.Config` when `TLSEnabled()`
- Test: unit (fake Executor records `PROXYSQL RELOAD TLS`; fingerprint compare paths; marker math incl. crash-window — copy the vars-marker test structure) + envtest (marker adoption; rotation with zero ready pods keeps marker unadvanced)

**Key design points to encode:** rotation is content-triggered (Secret watch already enqueues the cluster); verification dials the POD address (per-pod DNS), not the Service; kubelet propagation lag = bounded retry then fallback; the fallback annotation is separate from cnf-checksum (cnf text unchanged by rotation); operator-side dial must tolerate the OLD cert during the rotation window (CA pool contains old+new CA? — self-signed tier: CA is stable, only serving cert rotates → pool is just the CA; user/cert-manager tier: pool from ca.crt of the CURRENT secret, and handshake with InsecureSkipVerify=false against pod DNS names — SANs cover per-pod wildcard).

- [ ] Steps: unit tests red → implement client + engine → envtest specs → full suite green → commit in two logical commits (`feat(proxysqlclient): TLS dialing`, `feat(operator): restart-free TLS rotation with handshake verification`).

---

### Task 6: e2e scenario

**Files:** Create `test/e2e/scenarios/tls.sh`; register `scenario_tls` in `test/e2e/run.sh` after `scenario_external`.

Flow: cluster with `tls: {enabled: true}` (self-signed) + mysql backend + frontend user (copy `external.sh`'s backend boilerplate); extract the CA (`kubectl get secret pxc-tls-ca`); client pod connects `--ssl-mode=VERIFY_CA --ssl-ca=...` through 6033 and runs SELECT 1 (mount/pass the CA via a Secret volume or stdin-file — mirror how lib.sh runs clients, adapt); record served fingerprint (openssl s_client via a debug pod or the mysql client's cert output); force re-issue (delete `pxc-tls` Secret → operator re-issues from the SAME CA) → poll until the served fingerprint changes; assert restartCount unchanged and `tls-applied-hash` marker advanced; suite conventions: `wait_pod_ready`, `for _ in`, empty-or-zero jsonpath idiom, `bash -n` + shellcheck.

- [ ] Steps: write scenario → static verification (no kind locally; controller runs the suite) → commit `test(e2e): TLS scenario — verified client connect + restart-free rotation`.

---

### Task 7: Documentation

**Files:** `docs/reference/proxysqlcluster.md` (spec.tls tables + tls-applied-hash in annotations.md + TLSSecretError in status.md), new `docs/user-guide/tls.md` (+ README index entry), `docs/user-guide/security.md` rewrite of network/credential sections, `docs/reference/proxysqlconfig.md` (pgsqlServers.useSSL row).

Content contract: three tiers with precedence; backend-PKI-is-different warning; enable/disable = one structural restart, rotation restart-free; verification/fallback semantics; `--reload` interaction on persistent clusters; non-goals (frontend mTLS); every claim code-verified; bold-lead-in callouts.

- [ ] Steps: write → `make lint template` (controller re-runs if helm absent) → commit `docs: TLS management reference + user guide + security rewrite`.

---

### Task 8: PR

- [ ] Push `feat/tls`; PR titled `feat: TLS management — three-tier issuance, all-surface wiring, restart-free rotation (closes #53)` with spec link, validation evidence (unit/envtest/e2e incl. the restart-free rotation proof), the enable-restart upgrade note per the #58 policy; verify all CI green.
