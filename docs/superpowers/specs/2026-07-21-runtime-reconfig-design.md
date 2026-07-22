# Restart-free ProxySQLCluster reconfiguration (try-runtime → verify → fallback)

**Date:** 2026-07-21
**Status:** Approved design, pending implementation plan
**Decisions:** no hardcoded restart-required variable list — runtime read-back
is the oracle · cnf Secret always updated for bootstrap consistency · rolling
restart demoted from "any cnf change" to "verified-unappliable change only"

## Background

ProxySQL is designed to be fully reconfigurable at runtime over its admin
interface. The operator today does use that path for everything in
`ProxySQLConfig`, but `ProxySQLCluster` spec changes that alter the rendered
bootstrap `proxysql.cnf` take the blunt path: the cnf checksum is annotated on
the pod template (`proxysql.com/cnf-checksum`), so *any* cnf content change
triggers a rolling restart — even a single runtime-settable variable.

This design makes the reconciler try the admin-SQL path first and restart only
when a change verifiably does not take effect at runtime.

## Reconcile flow

When the rendered cnf differs from the current `-cnf` Secret content:

1. **Update the Secret unconditionally.** A future pod (re)start must
   bootstrap with the new config regardless of how the live pods were
   updated.
2. **Decouple the pod-template annotation from raw cnf content.** The
   annotation becomes the hash of the *last restart-applied* cnf. The
   reconciler tracks it explicitly (annotation on the StatefulSet,
   `proxysql.com/cnf-restart-checksum`) instead of recomputing it from the
   live Secret. On upgrade, an STS without the new annotation adopts its
   existing `proxysql.com/cnf-checksum` pod-template value as the initial
   restart-applied hash, so the upgrade itself never triggers a restart.
3. **Classify the change.** Diff old→new rendered content:
   - Changes confined to `*_variables` sections (admin/mysql/pgsql global
     variables) → candidate for runtime application.
   - Anything else — datadir layout, interfaces/port lists, structural
     changes (e.g. logging sidecar wiring), credential changes that alter the
     operator's own connection parameters — → restart path directly.
4. **Try runtime.** For each changed variable, on every ready replica:
   `UPDATE global_variables SET variable_value=? WHERE variable_name=?`, then
   one `LOAD ADMIN|MYSQL|PGSQL VARIABLES TO RUNTIME` + `SAVE ... TO DISK` per
   affected family. Reuses the `Executor` interface; no concrete client
   dependency in the sync path.
5. **Verify by read-back.** `SELECT variable_name, variable_value FROM
   runtime_global_variables WHERE variable_name IN (...)` on each replica and
   compare. ProxySQL accepts `SET` for restart-required variables (e.g.
   `mysql-threads`, `mysql-interfaces`) without changing the runtime value —
   the mismatch is the signal.
6. **Fallback.** If any variable on any replica fails read-back, set the
   restart-applied hash to the new cnf hash → normal rolling restart. The
   `Progressing` condition message names the variables that forced it.
   If all replicas verify, the restart hash stays put and no pods restart;
   `Progressing` reports `RuntimeApplied`.

Replicas that are not ready during the runtime pass are ignored — they will
bootstrap from the updated cnf Secret when they (re)start, which converges to
the same state.

## Interaction with ProxySQLConfig variables

`ProxySQLConfig.spec.*Variables` already writes the same admin tables at
runtime. Precedence is unchanged: cluster-spec cnf provides bootstrap
defaults; `ProxySQLConfig` sync runs after and wins for keys it defines. The
runtime-apply pass uses the same SQL shape, so a key set in both places
converges to the `ProxySQLConfig` value exactly as it does today after a
restart.

## Failure modes

- **Admin unreachable on some replica** during the runtime pass → requeue and
  retry (existing sync retry/requeue behavior); the change is not lost
  because the Secret is already updated and the diff re-derives from
  Secret-vs-rendered state. No restart is forced by mere unreachability.
- **Partial application** (some replicas verified, one failed read-back) →
  fallback restart of the StatefulSet; runtime-applied replicas restart into
  the identical config from the cnf, so no divergence survives.
- **Operator restart mid-pass** → idempotent: the diff and verify re-run from
  observed state.

## Testing

- Builder/unit: cnf render diffing (variables-only vs structural), restart
  hash adoption for legacy STS annotations.
- Fake-executor tests: emitted UPDATE/LOAD/SAVE sequence, read-back verify,
  mismatch detection.
- envtest: variables-only change updates Secret + syncs without changing the
  pod-template annotation; structural change bumps it; verified-mismatch
  (fake) bumps it.
- e2e (kind): flip `mysql-max_connections` on `ProxySQLCluster` → value
  visible via admin on all replicas, pod restart count unchanged; flip a
  restart-required variable → rolling restart observed, new value present
  after restart.

## Acceptance

- A runtime-settable variable change on `ProxySQLCluster` reaches all
  replicas with zero pod restarts; a restart-required change still rolls the
  StatefulSet; both paths surface distinct `Progressing` messages; upgrade
  from the previous annotation scheme does not restart existing clusters.
