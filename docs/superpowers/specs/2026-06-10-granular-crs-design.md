# Granular CRs (ProxySQLUser, ProxySQLQueryRule): Design

**Date:** 2026-06-10
**Status:** Approved design, pending implementation plan
**Issue:** #23 (roadmap §3.2)
**Decision:** one entity per CR · all sources targeting a cluster merge into
one `Desired` → one hash → one write-to-all pass · the sync engine becomes a
cluster-keyed aggregator · document CR wins conflicts, the loser goes
`Degraded` · deletion re-pushes the merged state *without* the CR's rows

## Context

`ProxySQLConfig` is a single document: fine for one team, but multi-tenant
clusters need per-object RBAC — a team owns its users and query rules without
write access to the whole config. The roadmap's standing decision holds:
`ProxySQLConfig` remains the primary, recommended single-team API; granular
CRs are *additive composition into the same sync engine*, not a second path.

## API shape

One entity per CR — per-object RBAC is the whole point, and single-entity CRs
make ownership, conflict attribution, and deletion semantics row-precise:

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLUser
metadata: {name: team-a-app}
spec:
  clusterRef: {name: proxysql}
  protocol: mysql                  # mysql | pgsql
  username: app_a
  passwordSecretRef: {name: team-a-app, key: password}
  defaultHostgroup: 1
  # active, maxConnections, useSSL, defaultSchema, transactionPersistent,
  # comment — same fields as the document's user entries
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLQueryRule
metadata: {name: team-a-reads}
spec:
  clusterRef: {name: proxysql}
  protocol: mysql
  ruleId: 1100
  matchDigest: "^SELECT"
  destinationHostgroup: 1
  apply: true
  # full mysql_query_rules / pgsql_query_rules field set, as in the document
```

Field schemas are shared with the document CR's entry types (same Go structs
where possible), so a row means the same thing regardless of which CR carried
it.

## Aggregation: the sync engine becomes cluster-keyed

Today `ProxySQLConfigReconciler` reconciles per-`ProxySQLConfig`: build
`Desired` from that one document, hash, push, write that config's status.
With multiple source kinds the per-config key breaks down, and the deciding
case is **granular CRs with no `ProxySQLConfig` at all** — a team-owned
`ProxySQLUser` must sync standalone. Keeping per-config reconcile would force
a synthetic empty document (or a parallel reconciler per granular kind, i.e.
the second sync path we ruled out). So:

- **Reconcile key changes to the cluster**: requests are keyed
  `namespace/clusterName` (the `ProxySQLCluster`'s NamespacedName). The
  reconciler lists all `ProxySQLConfig` + `ProxySQLUser` +
  `ProxySQLQueryRule` in the namespace with that `clusterRef`, resolves
  Secrets, merges into **one `Desired`, one fingerprint, one write-to-all**.
  `proxysqlclient` (`Sync`, `ReadRuntime`, `Drift`) is untouched.
- **Watches**: all three source kinds map to their cluster key; the existing
  Cluster/Pod/Secret watches re-target the same key. `configsForSecret`
  extends to `ProxySQLUser.passwordSecretRef`.
- **Aggregate sync state moves to `ProxySQLCluster.status.configSync`**
  (`lastAppliedHash`, `lastSyncTime`, `syncedReplicas`, `driftedReplicas`,
  `shunnedBackends`, `lastRuntimeCheckTime`): these describe the cluster's
  merged config, and no single source CR is a correct home for them once
  several contribute (and none may exist). The cluster reconciler and the
  config-sync reconciler write disjoint status fields; optimistic
  concurrency handles overlap. Per-source CRs keep `observedGeneration` and
  conditions only. (`ProxySQLConfig.status` sync fields are kept populated
  for one release for compatibility, then deprecated.)
- Determinism: sources are merged in a fixed order (documents by name, then
  granular CRs by creationTimestamp, name as tie-break) so the fingerprint
  is stable across reconciles.

## Conflict rule

A granular CR claiming an identity already defined elsewhere loses,
deterministically; identity keys are `(protocol, username)` for users and
`(protocol, ruleId)` for rules:

1. **Document beats granular**, always — the document is the cluster-wide
   authority a platform team owns.
2. Granular vs granular: oldest `creationTimestamp` wins; name as tie-break.

Detection is reconcile-time, during the merge (CEL cannot see across
objects). The loser is **excluded entirely** from `Desired` — never
partially applied — and gets `Degraded=True` plus `Ready=False`:

```
type: Degraded, reason: Conflict
message: username "app_a" (mysql) is already defined by ProxySQLConfig
         "pxcfg"; this ProxySQLUser is excluded from sync
```

The winner is unaffected and stays `Ready`. Resolution is a normal edit
(rename/renumber or delete the loser), which re-enqueues the cluster key.

## Deletion semantics

Each granular CR carries the cleanup finalizer (reusing the #14 machinery:
same `proxysql.com/config-cleanup` name, same `proxysql.com/skip-cleanup`
escape hatch, same never-wedge rules for absent cluster/Secret). The cleanup
action differs crucially from the document's: **re-push the merged `Desired`
built without the deleting CR's rows — not a table wipe.** Other tenants'
rows must survive a neighbor's deletion. The cluster-keyed engine gives this
for free: the finalizer holds until one full write-to-all of the
CR-excluded `Desired` succeeds, then releases. When the last source of a
cluster is deleted the merge degenerates to an empty `Desired`, which is
exactly today's document-deletion wipe — consistent by construction. A CR
that was a conflict loser (never applied) releases immediately.

## Validation and admission

- **Per-CR CEL** stays cheap because each CR is one entity: required fields,
  `ruleId >= 1`, `protocol` enum, exactly-one-match-criterion on rules —
  mirroring the document's existing per-entry validation.
- **Cross-CR duplicates are reconcile-time only**: admission cannot CEL
  across objects, and a webhook that lists the namespace on every CREATE is
  the cert-manager dependency we keep declining (roadmap §1.4 stance). The
  conflict condition above is the contract.
- The document CR's existing intra-list CEL rules are unchanged.

## Status per CR

`ProxySQLUser`/`ProxySQLQueryRule` status: `conditions` (`Ready` with reason
`Synced`/`SecretMissing`/`ClusterMissing`; `Degraded` with reason
`Conflict`), `observedGeneration`. Printer columns: `Cluster`, `Ready`,
`Reason`. "Synced to how many replicas" is read from
`ProxySQLCluster.status.configSync` — per-CR replica counts would just
duplicate the aggregate N copies.

## Sizing: reused vs new

Reused as-is: `proxysqlclient` (Sync/Executor/ReadRuntime/Drift), the
write-to-all + informed-resync loop, hash short-circuit, finalizer policy,
Secret-watch mapping pattern, condition helpers.

New: two CRD types (+ `make manifests && make sync-crds`, chart CRD copies,
RBAC markers), the cluster-keyed request mapping, the merge/conflict module
(pure, unit-testable like builders), `ProxySQLCluster.status.configSync`,
per-source status writers, envtest coverage (document+granular merge;
conflict goes `Degraded`; deleting a granular CR removes only its rows;
standalone granular CR with no document syncs).

Prerequisite ordering:

1. Refactor the existing reconciler to the cluster key with `ProxySQLConfig`
   as the only source kind — behavior-neutral, lands alone, soaks in e2e.
2. Move aggregate sync status to `ProxySQLCluster.status.configSync`
   (compat-populating the old fields).
3. Add the two CRDs + merge/conflict + finalizers + status.
4. If #22 lands first, its `backendSources` resolution simply becomes
   another input to the same merge — the two designs share the aggregator.

## Non-goals

- A granular `ProxySQLServer`/backend CR — backend auto-discovery (#22)
  covers backends; hand-listed servers stay in the document.
- Granular CRs for variables, replication hostgroups, or hostgroup
  attributes — cluster-scoped concerns belong to the document.
- RBAC documentation templates / tenant Role examples — docs work, tracked
  separately.
- Cross-namespace `clusterRef` — same-namespace only, as today.
