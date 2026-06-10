# Roadmap: Hardening, Feature Depth, and Differentiators

**Date:** 2026-06-10
**Status:** Approved design, pending implementation plan
**Decisions:** document CR (`ProxySQLConfig`) remains the primary API now, with
granular CRs added later as composition · production hardening before features

## Background

The operator today (API group `proxysql.com/v1alpha1`) ships two CRDs
(`ProxySQLCluster`, `ProxySQLConfig`), write-to-all sync with periodic drift
resync, envtest reconciler tests, full CI (lint, kubeconform, Trivy, kind e2e,
nightly backend examples), three Helm charts, release automation, and
architecture docs.

A review of the current implementation against the wider ProxySQL feature
surface identified the gaps this roadmap addresses: no cleanup on
`ProxySQLConfig` deletion, no Secret-rotation triggers, no runtime status
read-back, late (reconcile-time) validation of invalid specs, a narrow
query-rule model, and no backend auto-discovery.

## Principles

- Every change conforms to existing conventions: pure builders, `Executor`
  interface in `proxysqlclient`, PSA `restricted`, `*bool` for default-true
  booleans, CRDs synced via `make sync-crds`.
- `ProxySQLConfig` (the document model) remains the primary, fully supported
  API. Granular CRs arrive later as additive composition, not replacement.

## Milestone 1 — v0.2.0 "Trustworthy lifecycle" (hardening)

Ordered by risk reduction per effort.

### 1.1 ProxySQLConfig deletion finalizer

On `ProxySQLConfig` deletion: clear the managed admin tables on all ready
replicas (`DELETE FROM <table>` for every table the config managed, then
`LOAD ... TO RUNTIME; SAVE ... TO DISK`), then remove the finalizer.

- Finalizer name: `proxysql.com/config-cleanup`.
- Opt-out annotation `proxysql.com/skip-cleanup: "true"` skips the SQL and
  removes the finalizer immediately (emergency unblock, e.g. cluster already
  gone).
- If the referenced cluster or its pods no longer exist, cleanup is a no-op
  and the finalizer is removed (deletion must never wedge on an absent
  cluster).
- Today deletion is silently a no-op on the proxies; this closes that gap.
  (Finalizer RBAC already exists but is unused.)

### 1.2 Secret watches

`ProxySQLConfigReconciler` adds `Watches(&corev1.Secret{}, ...)` with a mapper
that resolves Secrets back to ProxySQLConfigs via:

- `spec.mysqlUsers[].passwordSecretRef` / `spec.pgsqlUsers[].passwordSecretRef`
- the referenced cluster's admin auth Secret

Password rotation then converges immediately instead of waiting for the
2-minute drift resync (which stays as the safety net).

### 1.3 Runtime status read-back

After each successful sync, read back from each replica:

- `runtime_mysql_servers` / `runtime_pgsql_servers` (incl. SHUNNED status)
- `runtime_mysql_users` / `runtime_pgsql_users` (presence only, never hashes)
- `stats_mysql_connection_pool` (connections used/free, latency)
- monitor replication-lag data where available

Surface in `ProxySQLConfig.status`: per-replica drift summary, shunned-backend
count, max replication lag. Reads go through the existing `Executor` interface
so `sync_test.go`-style fakes cover them. This makes `kubectl get pxcfg`
diagnostic and turns the blind periodic re-push into informed reconciliation
(re-push only replicas that actually drifted; full re-push remains the
fallback).

### 1.4 Admission validation — CEL first

Add CEL validation rules (`+kubebuilder:validation:XValidation`) for:

- duplicate `ruleId` within `mysqlQueryRules` / `pgsqlQueryRules`
- duplicate (hostgroup, hostname, port) within `mysqlServers` / `pgsqlServers`
- duplicate usernames within `mysqlUsers` / `pgsqlUsers`

Cross-resource checks (e.g. `pgsqlServers` set while the referenced cluster has
pgsql disabled) cannot be expressed in CEL; surface those as a `Degraded`
condition at reconcile time. A validating webhook is added **only if** we hit a
rule CEL cannot express and a condition is insufficient — avoiding the
cert-manager dependency is the default stance.

### 1.5 Finish the test matrix

- Implement the 4 stubbed nightly examples: `percona-ps`, `percona-pxc`,
  `oracle-mysql-operator`, `crunchy-pgo`.
- Real e2e assertions in `test/e2e`: apply ProxySQLCluster + ProxySQLConfig,
  query the admin port, assert table rows; delete the config, assert cleanup
  (exercises 1.1); rotate a password Secret, assert convergence (exercises
  1.2); check status fields (exercises 1.3).

## Milestone 1.5 — v0.2.5 "Platform integration surface"

Requirements gathered from control-plane platforms that provision ProxySQL
clusters programmatically: create the CR, poll status, hand endpoints to
applications. These make the operator consumable by such platforms without
them having to interrogate Services and StatefulSets directly.

### 1.5.1 Aggregate phase and endpoints in ProxySQLCluster status

- `status.phase`: one of `Pending | Creating | Running | Updating | Degraded |
  Failed`, derived from the conditions and StatefulSet state the operator
  already tracks. Conditions remain the source of truth; phase is a
  convenience projection for dashboards and external pollers.
- `status.endpoints`: in-cluster DNS endpoints (`host:port`) for every enabled
  surface — `mysql`, `pgsql`, `admin`, `web` (1.5.2), `metrics`.
- `status.updatedReplicas` (from the StatefulSet), completing the
  replicas/ready/updated triple.
- New printer column `Phase`.

### 1.5.2 ProxySQL web UI exposure

New `protocols.web {enabled, port}` (default off, port 6080) on
ProxySQLCluster: enables ProxySQL's built-in web/REST stats UI, adds the
container and Service port, and surfaces it via `status.endpoints.web`.

### 1.5.3 Admin Secret schema compatibility

Accept a user-supplied admin Secret in the common `username`/`password` shape
in addition to the operator-minted
`admin-password`/`radmin-password`/`monitor-password` schema. Resolution order
and precedence documented in `docs/architecture.md`; a clear condition surfaces
when neither schema matches.

### 1.5.4 Networking knobs

- `service.annotations` (cloud load-balancer configuration)
- `service.sessionAffinityTimeoutSeconds`
- `networking.tcpKeepalive {time, interval, probes}` — pod-level sysctls; must
  be reconciled with the PSA `restricted` stance (only the kernel-namespaced
  `net.*` sysctls allowed under restricted PSA are acceptable; anything else
  is rejected at admission).

### 1.5.5 Logging sidecar (design + implement)

Optional log-shipping sidecar (Fluent Bit) on ProxySQLCluster: sinks
`stdout | s3 | http`, optional ProxySQL query-log collection. Gets a short
design round first (volume handling under `readOnlyRootFilesystem`, image
pinning, resource defaults), then lands behind a default-off toggle.

### 1.5.6 External-backend failover (design decision)

For backend databases *outside* the cluster (not covered by 3.1's CR
watching), decide between integrating an external topology manager
(Orchestrator-style, seeded discovery) and extending the operator's own use of
`mysql_replication_hostgroups` + read_only monitoring. Design-only in this
milestone; the outcome feeds 3.1's scope.

## Milestone 2 — v0.3.0 "Feature depth"

### 2.1 Richer query rules

Extend `MySQLQueryRule` (and pgsql equivalent where ProxySQL supports it) with:
`replacePattern`/`replaceWith` (rewriting), `mirrorHostgroup`, `timeout`,
`delay`, `errorMessage`, `flagIn`/`flagOut` (rule chaining), `log`, and query
cache fields (`cacheTTL`, `cacheEmptyResult`). Same DELETE/INSERT/LOAD/SAVE
sync pattern — additional columns in `sync.go` plus `Desired` fields in
`types.go`.

### 2.2 Hostgroup attributes and auth plugins

- `mysqlHostgroupAttributes` list syncing to `mysql_hostgroup_attributes`.
- Per-user `authPlugin` field (`mysql_native_password`, `caching_sha2_password`,
  `sha256_password`).

### 2.3 Bootstrap cnf moves to a Secret

The rendered `proxysql.cnf` (which embeds admin/radmin/monitor passwords) moves
from ConfigMap to Secret. Coordinated change: builder, StatefulSet volume,
checksum annotation, chart templates, `docs/architecture.md`. This closes the
documented standing item.

## Milestone 3 — v0.4.0 "Differentiators + hybrid API"

### 3.1 Backend auto-discovery

The strongest differentiator: watch backend database CRs and feed
`mysql_servers`/`pgsql_servers` automatically, including writer-failover
updates.

- API shape: a `backendSource` stanza on `ProxySQLConfig` (or a small
  dedicated CR — decided in its own design round) selecting a backend kind +
  name/labels and mapping roles to hostgroups.
- Initial backends: **CloudNativePG and MariaDB Operator** (the two live
  nightly examples — test infrastructure already exists). Percona PXC,
  Oracle InnoDBCluster, Crunchy PGO follow.
- This item gets its own brainstorm/spec before implementation.

### 3.2 Granular CRs, additively (hybrid API decision)

Introduce `ProxySQLUser` and `ProxySQLQueryRule` CRs as *composable sources
feeding the same sync engine*:

- Internally, all sources (ProxySQLConfig document + granular CRs referencing
  the same cluster) merge into one `Desired` struct per cluster → one hash →
  one write-to-all pass. No second sync path.
- Conflict rule: a granular CR claiming a username/ruleId already defined in a
  ProxySQLConfig (or another granular CR) is rejected to `Degraded`; the
  document CR wins. Deterministic, surfaced in conditions.
- Each granular CR reuses the Milestone-1 machinery: cleanup finalizer, secret
  watch, status read-back.
- Purpose: per-object RBAC for multi-tenant scenarios. ProxySQLConfig remains
  fully supported and remains the recommended single-team API.

## Testing strategy

- Every milestone item lands with unit tests (builders / sync fakes) and,
  where it changes reconcile behavior, envtest coverage.
- Milestone 1 deliberately pairs lifecycle features (1.1–1.3) with the e2e
  assertions that prove them (1.5).
- Auto-discovery (3.1) is validated against the nightly example backends.

## Sizing and order

M1 is the largest (touches both reconcilers, needs e2e). M1.5 is mostly
status/builder plumbing plus one designed feature (logging sidecar) and one
design round (external failover); it lands between the hardening and feature
milestones because downstream platforms block on it. M2 is mostly mechanical
column-plumbing. M3.1 is the one genuinely novel design effort and gets its
own spec. M3.2 reuses M1 machinery.
