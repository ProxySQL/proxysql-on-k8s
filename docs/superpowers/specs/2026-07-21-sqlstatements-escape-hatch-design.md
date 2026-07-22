# Raw admin-SQL escape hatch: `ProxySQLConfig.spec.sqlStatements`

**Date:** 2026-07-21
**Status:** Approved design, pending implementation plan
**Decisions:** idempotent desired-state semantics (not one-shot) · field on
`ProxySQLConfig` (no new CRD) · no SQL parsing or allow-listing · executes
after structured config in the same sync pass

## Background

ProxySQL is fully runtime-configurable over its admin interface, but the
operator only models a subset of that surface as structured CRD fields
(servers, users, query rules, hostgroups, variables). Anything else — cache
flushes, admin commands, settings not yet modeled — requires bypassing the
operator with a manual admin connection, which the drift resync may then
fight.

`spec.sqlStatements` gives users a declarative escape hatch: raw admin SQL
that the operator itself executes on every replica, inside the same sync
machinery that pushes the structured config.

## API

New optional field on `ProxySQLConfigSpec`:

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
spec:
  clusterRef: {name: proxysql}
  sqlStatements:
    - "UPDATE global_variables SET variable_value='250' WHERE variable_name='mysql-max_connections'"
    - "LOAD MYSQL VARIABLES TO RUNTIME"
    - "PROXYSQL FLUSH QUERY CACHE"
```

- `sqlStatements []string`, `+optional`, no admission-time validation beyond
  the schema (strings, non-empty list items). CRD description documents the
  idempotency requirement and footguns.
- Statements are opaque to the operator: no parsing, rewriting, or
  allow-listing. This is an explicitly sharp tool.

## Semantics

- **Desired-state, not one-shot.** Statements are carried in
  `proxysqlclient.Desired` (new `SQLStatements []string` field) and executed
  by `Sync` on every ready replica on every sync pass — including new or
  restarted replicas and drift-triggered resyncs. Users MUST write idempotent
  SQL; re-execution is normal operation, not a bug.
- **Ordering.** Statements run *after* all structured tables and variables in
  the listed order. The operator does not append `LOAD ... TO RUNTIME` for
  them — if a statement's effect needs a load/save, the user includes those
  statements explicitly.
- **Failure handling.** The first failing statement fails that replica's sync
  pass. Existing condition machinery reports it (`PartialSync`/`Degraded`
  with the SQL error); `status.syncedReplicas` excludes the failed replica.
  No new status fields.
- **Hash participation.** Statement text is part of the config hash, so any
  edit re-triggers a sync and is reflected by `syncedReplicas`.
- **Cleanup.** The deletion finalizer does NOT attempt to reverse
  `sqlStatements` (their effects are opaque). Documented limitation.
- **Drift.** Runtime read-back/drift detection continues to cover only
  structured tables; raw-SQL side effects are invisible to it. Documented
  limitation.

## Security notes (documented, not enforced)

- Statements run with the operator's admin (`radmin`) credentials; anyone who
  can write `ProxySQLConfig` can already reshape routing, so this adds no new
  trust boundary — but statements that break admin connectivity (e.g.
  changing `admin-admin_credentials`) will lock the operator out until the
  cnf-based credentials are restored by pod restart. The user guide calls
  this out with a warning box.
- The multi-tenant story (granular CRs, #23) deliberately does NOT get raw
  SQL; the field lives only on the cluster-scoped-trust document CR.

## Testing

- `sync_test.go` fake-executor tests: statements emitted verbatim, in order,
  after structured config; failure surfaces as sync error for that replica;
  hash changes when statements change.
- envtest: not applicable — the reconciler dials real ProxySQL directly
  (`applyToReplicas` has no executor seam), matching the existing suite,
  which covers finalizer/status paths only. Execution semantics live in the
  fake-executor unit tests; the success/`Degraded` surface is exercised by
  the kind e2e, same as for structured config.
- e2e (kind suite): statement executes on the replica with a
  runtime-verifiable effect; edit → re-sync observed via `lastAppliedHash`.
- Docs: reference page for the field + user-guide section with idempotency
  and lockout warnings.

## Acceptance

- Statements execute write-to-all with the documented ordering and failure
  semantics, `make manifests && make sync-crds` clean, all existing tests
  pass unchanged (`Executor` interface untouched).
