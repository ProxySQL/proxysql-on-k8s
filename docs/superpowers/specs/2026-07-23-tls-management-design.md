# TLS management: three-tier issuance, all-surface wiring, restart-free rotation

**Date:** 2026-07-23
**Status:** Approved design, pending implementation plan
**Decisions:** three-tier issuance (user Secret > cert-manager issuerRef >
operator self-signed) · v1 covers all three surfaces (frontend, backend,
admin/cluster) · rotation = `PROXYSQL RELOAD TLS` + handshake verification +
rolling-restart fallback · opt-in and permissive by default (`spec.tls`
absent ⇒ byte-identical rendering) · frontend/admin certs reach ProxySQL via
datadir SYMLINKS into the Secret mount (verified: proxysql:3.0 has no
frontend/admin cert-path variables — certs are fixed datadir names; an
init container seeds idempotent symlinks, kubelet's atomic Secret updates
propagate through them, RELOAD TLS re-reads — restart-free rotation
preserved)

## Background

The operator has no TLS story: users hand-wire certs through variables and
their own Secrets. The Percona operators issue and rotate certs but roll
pods on every rotation and copy certs into the datadir (their ProxySQL
vintage lacked configurable frontend cert paths). We can do better on the rotation
side: restart-free rotation using `PROXYSQL RELOAD TLS` verified by a real
handshake — the same try-verify-fallback philosophy as the runtime
reconfiguration feature. A live probe of the shipped `proxysql:3.0` image
(2026-07-23) found NO frontend/admin cert-path variables — frontend/admin
certs are the fixed datadir names `proxysql-{ca,cert,key}.pem`, which
`PROXYSQL RELOAD TLS` re-reads. The delivery mechanism is therefore
datadir symlinks into the Secret mount (seeded by an init container,
idempotent, PVC-safe): kubelet updates the mounted Secret atomically, the
symlinks keep resolving, and RELOAD TLS picks up the new cert with no
copy step and no sidecar.

## API

```yaml
spec:
  tls:
    enabled: true              # plain bool, default off. Absent/false ⇒ the
                               # operator renders exactly what it renders
                               # today (golden-pinned; no upgrade restart).
    secretName: ""             # tier 1: user-provided kubernetes.io/tls
                               # Secret (tls.crt/tls.key, plus ca.crt).
                               # Wins whenever set.
    issuerRef:                 # tier 2: cert-manager. Used when secretName
      name: ""                 # is empty and name is set. kind defaults to
      kind: Issuer             # Issuer; group to cert-manager.io.
      group: cert-manager.io
    duration: "2160h"          # certificate lifetime (90d default);
    renewBefore: "720h"        # renewal window (30d default). Apply to
                               # tiers 2 and 3.
    extraSANs: []              # additional DNS names / IPs (LB hostnames
                               # for the external Service, custom DNS).
    backend:                   # ProxySQL→database trust — a DIFFERENT PKI
      caSecretName: ""         # CA bundle for mysql-ssl_p2s_ca (+ pgsql
                               # equivalent). Unset ⇒ backend TLS variables
                               # not rendered.
      clientCertSecretName: "" # optional mTLS client cert/key for
                               # ssl_p2s_cert / ssl_p2s_key.
```

- **Tier resolution** (frontend/admin serving cert): `secretName` >
  `issuerRef` > self-signed. Tier 3 mints a CA into `<cluster>-tls-ca`
  (preserved across reconciles, like operator-managed auth Secrets) and
  issues the serving cert into `<cluster>-tls`; re-issues before
  `renewBefore` expires.
- **SANs**: `<name>`, `<name>.<ns>`, `<name>.<ns>.svc`,
  `*.<name>-headless.<ns>.svc` (per-pod), `<name>-external`, plus
  `extraSANs`.
- **The backend block is deliberately separate**: `ssl_p2s_ca` must trust
  the *database's* issuer, which is ours only when one PKI signs both
  sides. Conflating the two (a naive single-secret model) silently breaks
  verification against managed backends.
- `pgsqlServers` entries in `ProxySQLConfig` gain the `useSSL` field that
  `mysqlServers` already has; per-hop backend encryption remains explicit
  per server.

## Wiring (all three surfaces)

One read-only mount of the RESOLVED tls Secret (tier 1: the user's
Secret directly; tiers 2-3: `<cluster>-tls`) at `/etc/proxysql/tls`
(`tls.crt`, `tls.key`, `ca.crt`). Rendered into the bootstrap cnf **only
when `tls.enabled`** — all rendered TLS variables join the reserved-key set (like
credentials: not user-overridable via `spec.variables`, structural for
classification):

- **Frontend**: an init container (the cluster's own proxysql image, uid
  999) seeds idempotent symlinks `datadir/proxysql-{ca,cert,key}.pem →
  /etc/proxysql/tls/{ca.crt,tls.crt,tls.key}` before proxysql starts, so
  ProxySQL loads the operator-provided certs instead of auto-generating;
  `have_ssl` stays on for mysql/pgsql (permissive — TLS available, not
  required; a future `require` knob can enforce). First-boot autogen and
  persistent-datadir interactions are pinned by a live boot probe during
  implementation.
- **Admin + cluster**: ProxySQL's admin interface serves TLS capability
  from the same datadir certs; the operator dials 6032 with TLS when
  enabled. Cluster-peering encryption follows whatever the image
  supports (probe-verified during implementation; 3.0 exposes no
  dedicated cluster-SSL flag).
- **Backend**: `mysql-ssl_p2s_ca` (+ pgsql) from `backend.caSecretName`,
  `ssl_p2s_cert/key` from `backend.clientCertSecretName`, rendered only
  when the refs are set.
- **Operator client**: `proxysqlclient.New` gains TLS options — a
  per-cluster registered `tls.Config` with the CA pool read from the
  Secret. TLS-off clusters dial plaintext exactly as today (envtest
  unaffected).

## Rotation: reload → verify → fallback

Rotation changes Secret *content*, not cnf *text*, so it is invisible to
the cnf/structural machinery by design. Flow:

1. The operator (already watching Secrets) sees the tls Secret change and
   compares a new object-level marker `proxysql.com/tls-applied-hash`
   (same crash-safe pattern as the vars/structural markers) against the
   Secret content hash.
2. Per ready replica: issue `PROXYSQL RELOAD TLS` on the admin interface,
   then **verify by handshake** — dial 6033 (and 6032) with `crypto/tls`,
   compare the served leaf certificate fingerprint against the Secret's
   cert. Kubelet mount-propagation lag is absorbed by bounded retry.
3. All replicas verified → marker advances; **zero restarts**.
4. A replica that fails verification within the bounded window → rolling
   restart via a dedicated pod-template annotation bump (the cnf hash
   cannot carry this — rotation doesn't change the cnf), with the
   `Progressing`/`Degraded` message naming the replica.
5. cert-manager renewals ride the same path automatically. Failure to
   resolve a referenced Secret ⇒ `Degraded=True` reason `TLSSecretError`,
   non-wedging (same contract as `ExternalServiceError`).

## Enabling/disabling semantics

- Enabling TLS on a live cluster changes the pod template (init
  container + Secret mount; backend refs additionally add cnf variables)
  ⇒ one documented rolling restart driven by the StatefulSet diff.
  Rotation thereafter is restart-free. Disabling likewise restarts;
  on PERSISTENT datadirs the retained pem symlinks dangle after the
  mount disappears — behavior pinned by a dedicated probe (Task 5/6)
  and documented (cleanup caveat if the probe shows breakage).
- With persistence enabled, `--reload` merges the TLS variables over
  `proxysql.db` on boot (already-shipped semantics), so restarts converge.

## Testing

- Golden: byte-identical rendering with `tls` absent (pinned).
- Unit: tier resolution precedence; SAN set; cnf rendering per surface;
  reserved-key protection of TLS variables; fingerprint comparison.
- envtest: self-signed CA persistence across reconciles; `TLSSecretError`
  degraded/non-wedging/clear; cert-manager Certificate object creation
  (cert-manager CRDs installable in envtest as unstructured, else scoped
  to unit level — decide in the plan); marker adoption on upgrade.
- e2e (kind): self-signed cluster → client connects with
  `--ssl-mode=VERIFY_CA` against the operator CA through 6033; operator
  re-issues the cert → assert the new fingerprint is served with
  restartCount unchanged (the restart-free rotation headline); admin path
  covered implicitly (operator keeps syncing over TLS after rotation).
- Docs: `spec.tls` reference tables; user-guide TLS page; `security.md`
  network/credential rewrite; explicit non-goals (frontend client-cert
  auth/mTLS — future).

## Acceptance

- Zero-config TLS: `tls: {enabled: true}` alone yields a working
  self-signed setup — clients can verify against the published CA.
- Cert rotation (any tier) reaches all replicas with zero pod restarts in
  the verified path; an unverifiable replica falls back to a restart.
- `spec.tls` absent renders byte-identically to today (golden), and the
  operator↔ProxySQL admin channel works in both modes.
- All CRD validation at admission; `make manifests && make sync-crds`
  clean; existing tests unchanged.
