# Status, conditions, and reconcile behavior

Complete inventory of every condition type and reason the two reconcilers
emit, how `ProxySQLCluster.status.phase` is derived, the requeue cadences,
and the drift-resync (level-based self-healing) behavior of the
`ProxySQLConfig` reconciler. For day-2 interpretation and troubleshooting
see the [operations user guide](../user-guide/operations.md); field-level
status schemas live in the [ProxySQLCluster](proxysqlcluster.md#status) and
[ProxySQLConfig](proxysqlconfig.md#status) references.

All conditions carry `observedGeneration` set to the CR generation at the
time they were written.

## ProxySQLCluster conditions

| Type | Status | Reason | When |
|---|---|---|---|
| `Available` | `True` | `AllReplicasReady` | `readyReplicas == replicas` and `replicas > 0`. Message: `N/N replicas ready`. |
| `Available` | `False` | `ReplicasNotReady` | Any replica not ready. Message: `n/N replicas ready`. |
| `Progressing` | `True` | `Rolling` | Set together with `Available=False`; waiting for replicas. Message is `waiting for replicas`, unless this reconcile just decided a restart is needed for a `spec.variables` change, in which case it's `RestartRequired: <sorted variable names> (runtime read-back mismatch)` (a variable didn't take at runtime) or `RestartRequired: structural cnf change` (a non-variable cnf change: ports, credentials, `replicas`, logging sidecar, ...). The StatefulSet template diff is what actually drives the rollout either way; this only improves the message. |
| `Progressing` | `False` | `Steady` | Set together with `Available=True`; no rollout in progress. |
| `Progressing` | `False` | `RuntimeApplied` | A `spec.variables` change was pushed to every ready replica over the admin port and read back successfully — restart-free. Message: `RuntimeApplied: <sorted variable names>`. Takes priority over `Steady` for the reconcile in which it fires; nothing is rolling out, but it's worth surfacing what just changed. See [ProxySQLCluster reference](proxysqlcluster.md#configuration-changes-runtime-vs-restart). |
| `Degraded` | `True` | `AuthSecretError` | Auth-Secret resolution failed: external Secret missing, partial operator schema, schema mismatch, or invalid credential characters. The reconcile aborts (no resources are touched) and `phase` is set to `Degraded`. |
| `ServiceMonitorReady` | `True` | `Synced` | ServiceMonitor applied. |
| `ServiceMonitorReady` | `False` | `CRDNotInstalledOrFailed` | ServiceMonitor create/update failed — most commonly the `monitoring.coreos.com/v1` CRD is not installed. **Non-fatal**: the rest of the reconcile is unaffected. |
| `ServiceMonitorReady` | `False` | `OwnerRefError` | Setting the controller reference on the ServiceMonitor failed (non-fatal). |

Removal rules:

- `Degraded` is **removed** on every successful status update (i.e. whenever
  auth resolution and resource ensure-steps succeed).
- `ServiceMonitorReady` is **removed entirely** when the ServiceMonitor is
  not desired (`metrics.enabled: false` or
  `metrics.serviceMonitor.enabled: false`); any previously created,
  operator-owned ServiceMonitor is deleted best-effort.

### Phase derivation

`status.phase` is a coarse projection of StatefulSet state for dashboards
and external pollers; conditions are the source of truth.

| Phase | Rule (evaluated in order) |
|---|---|
| `Degraded` | Auth-Secret resolution failed (set before the StatefulSet is even examined). |
| `Pending` | StatefulSet missing / not yet created. |
| `Creating` | StatefulSet exists, `readyReplicas == 0`. Deliberately coarse: a previously running cluster in total outage also reads `Creating`. |
| `Running` | `readyReplicas == replicas` and update revision == current revision (or no update revision). |
| `Updating` | Everything else (partial readiness, rollout in flight). |
| `Failed` | Reserved for future positively-identified terminal states; never currently emitted. |

### ProxySQLCluster requeues and watches

The cluster reconciler sets no timed requeue; it is purely event-driven via
watches on the CR and its owned objects (StatefulSet, Services, Secrets,
PodDisruptionBudget). Errors requeue with controller-runtime's default
backoff.

## ProxySQLConfig conditions

| Type | Status | Reason | When | Requeue |
|---|---|---|---|---|
| `ClusterFound` | `True` | `Found` | `spec.clusterRef` resolved. | — |
| `ClusterFound` | `False` | `NotFound` | Target `ProxySQLCluster` does not exist in the namespace. `Ready=False/ClusterMissing` is set alongside. | 5s |
| `Ready` | `True` | `Synced` | Config applied to all ready replicas. Message: `config applied to N/N replicas`. | 30s |
| `Ready` | `False` | `ClusterMissing` | Companion to `ClusterFound=False/NotFound`. | 5s |
| `Ready` | `False` | `AdminSecretMissing` | The cluster's admin Secret could not be read. | 5s |
| `Ready` | `False` | `AdminSecretIncomplete` | The admin Secret matches no accepted credential schema (see [auth schemas](proxysqlcluster.md#accepted-secret-schemas-and-precedence)). | 5s |
| `Ready` | `False` | `UserSecretError` | A `passwordSecretRef` Secret or key is missing (message names the user). | 5s |
| `Ready` | `False` | `NoReadyReplicas` | No ready ProxySQL pods to push to. | 5s |
| `Ready` | `False` | `PartialSync` | Some replicas failed the push. Message: `synced n/N replicas (k re-push targets, m succeeded)`. `Degraded=True/SyncErrors` is set alongside. | 5s |
| `Progressing` | `False` | `Steady` | Set on every fully successful sync. (The reconciler never sets `Progressing=True`; absence or `False/Steady` are the only states.) | — |
| `Degraded` | `True` | `PgsqlDisabled` | The spec declares pgsql servers/users/rules but the referenced cluster has `protocols.pgsql` disabled. The push still happens (the admin tables exist regardless); this is a loud warning. | — |
| `Degraded` | `True` | `SyncErrors` | One or more replicas failed the SQL push; message aggregates per-address errors (truncated to 512 chars). When a pgsql mismatch coexists, its warning is folded into the message. | 5s |

Removal rules:

- `Degraded` is removed after a fully successful sync **unless** the
  `PgsqlDisabled` mismatch still applies.

### Requeue cadences

| Constant | Value | Used after |
|---|---|---|
| `requeueAfterSuccess` | **30s** | A successful sync, a no-op short-circuit, or a clean informed resync. Safety net for pod restarts that wiped runtime tables. |
| `requeueAfterTransient` | **5s** | Any transient failure: cluster missing, secrets missing/invalid, no ready pods, partial sync, or pending deletion-cleanup. |
| drift resync interval | **2m** default; operator flag `--config-resync-interval`, Helm value `configResyncInterval` | Bounds how long out-of-band runtime drift can persist (see below). |

### Watch-driven triggers

Besides the timed requeues, the config reconciler re-reconciles immediately
when:

- the target `ProxySQLCluster` changes (status flip, replica change, admin
  Secret rename);
- a Pod labeled `proxysql.com/cluster=<cluster>` changes (becomes Ready,
  restarts) — fresh pods get config without waiting for the 30s requeue;
- a Secret changes that is either a `passwordSecretRef` of the config or
  the admin Secret of the cluster it targets — password rotation converges
  on the next reconcile instead of the resync interval.

## The hash short-circuit and informed resync

Each reconcile computes a **fingerprint**: SHA-256 over the JSON-marshaled
resolved desired state (user passwords substituted from Secrets) plus the
sorted set of ready pod `IP:port` addresses. The SQL push is skipped only
when **all** of:

1. `status.lastAppliedHash` equals the fingerprint (so neither the spec, the
   resolved passwords, nor the pod membership changed — a recreated pod with
   a new IP busts the hash);
2. `status.syncedReplicas` equals the current ready-pod count;
3. `status.observedGeneration` equals the CR generation;
4. less than the drift resync interval has elapsed since
   `status.lastSyncTime`.

When only condition 4 fails (everything unchanged but the interval elapsed),
the reconciler performs an **informed resync** instead of a blind re-push:

1. Read runtime state back from every ready replica
   (`runtime_mysql_servers`, `runtime_mysql_users`,
   `runtime_mysql_query_rules`, and the pgsql equivalents — identity keys
   only, never passwords).
2. Update `status.lastRuntimeCheckTime`, `status.shunnedBackends`, and
   `status.driftedReplicas`.
3. If **no** replica drifted: verification counts as asserting desired
   state — `status.lastSyncTime` advances **without any SQL writes**, and
   the reconciler requeues in 30s.
4. If some replicas drifted (including any whose read-back failed — an
   unprovable replica is treated as drifted): the full Sync is pushed **only
   to those replicas**.

Drift comparison is keys-only (server `hostgroup:hostname:port`, usernames,
rule IDs); attribute-level changes are carried by the spec-hash path. A
`SHUNNED` backend is *present*, not drifted. Tables excluded from drift
detection (`mysql_replication_hostgroups`, `mysql_hostgroup_attributes`,
`proxysql_servers`, variables) are still re-asserted whenever any drift
triggers a push, because Sync always writes every table — see
[admin-tables.md](admin-tables.md#drift-detection-coverage).

### What `lastSyncTime` means

`lastSyncTime` is the last time the operator **asserted** desired state —
not necessarily the last time it wrote SQL. It advances when:

- a push to all target replicas succeeds, **or**
- an informed resync verifies every replica converged with zero writes.

So on a quiet, converged cluster `lastSyncTime` advances roughly every
resync interval, confirming the operator is actively verifying, while the
`Last-Sync` printer column stays fresh.
