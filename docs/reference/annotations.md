# Annotations, labels, finalizers, and owned-object names

Reference for every annotation, label, and finalizer the operator reads or
writes, plus the deletion (wedge) policy of the `ProxySQLConfig` finalizer
and the names of the objects a `ProxySQLCluster` owns. For day-2 procedures
(stuck deletion, forced restarts) see the
[operations user guide](../user-guide/operations.md).

## Annotations

### `proxysql.com/skip-cleanup` (user-set, on ProxySQLConfig)

| | |
|---|---|
| Set on | a `ProxySQLConfig` object (metadata.annotations) |
| Value | the exact string `"true"` (anything else is ignored) |
| Read by | the config reconciler's finalize path |

When a `ProxySQLConfig` is deleted, the operator normally clears the managed
admin tables on every ready replica before releasing the finalizer. With
this annotation set to `"true"`, the finalizer is released **without any SQL
cleanup**. It is the escape hatch for every stuck-deletion case — most
importantly a cluster that exists but will never have ready pods again:

```bash
kubectl annotate proxysqlconfig <name> proxysql.com/skip-cleanup=true
kubectl delete proxysqlconfig <name>
```

### `proxysql.com/cnf-checksum` (operator-set, on the pod template)

| | |
|---|---|
| Set on | the StatefulSet pod template (so on every ProxySQL pod) |
| Value | deterministic SHA-256 over **every key** of the `<cluster>-cnf` Secret (keys sorted, key and value length-prefixed) |
| Purpose | a change to `proxysql.cnf` *or* `fluent-bit.conf` that changes this annotation changes the pod template, which triggers a StatefulSet rolling restart |

This key is **reserved**: the operator writes it *after* merging
`spec.podAnnotations`, so a user-supplied entry with the same key is always
overwritten and can never clobber the rollout trigger. Don't set it — it
carries no user-configurable meaning.

**Exception:** a `proxysql.cnf` change confined to `spec.variables` *values*
(no key added or removed) does not necessarily change this annotation — the
operator tries a restart-free runtime apply first and only falls back to
updating this annotation (and thus restarting) if that fails. The
runtime-vs-restart classification diffs `proxysql.cnf` only; for example, a
`logging.queryLog` toggle restarts because it adds/removes the `eventslog_*`
lines in `proxysql.cnf` itself. See [runtime vs. restart
semantics](proxysqlcluster.md#configuration-changes-runtime-vs-restart).

### `proxysql.com/vars-applied-hash` (operator-set, on the StatefulSet object)

| | |
|---|---|
| Set on | the StatefulSet's own `metadata.annotations` — **not** the pod template, so setting it never triggers a rollout |
| Value | SHA-256 over the sorted `key=value` lines of **all runtime-appliable variables parsed from the rendered `proxysql.cnf`** — template-rendered defaults (`threads`, `cluster_check_*`, `eventslog_*`, monitor credentials, …) as well as `spec.variables` entries; only bootstrap-structural lines (`admin_credentials`, ifaces/interfaces) are excluded — as last successfully applied, whether via a restart-free runtime push or a restart |
| Purpose | closes the crash-safety window between the cnf Secret update and the runtime SQL push: if the operator dies after writing the Secret but before confirming the admin-port push, the mismatch between this annotation and the new cnf's variable set forces a fresh push attempt on the next reconcile, instead of silently assuming the old push succeeded |

Not user-configurable; see [runtime vs. restart
semantics](proxysqlcluster.md#configuration-changes-runtime-vs-restart) for
when this updates versus `proxysql.com/cnf-checksum`.

### `proxysql.com/structural-applied-hash` (operator-set, on the StatefulSet object)

| | |
|---|---|
| Set on | the StatefulSet's own `metadata.annotations` — **not** the pod template, so setting it never triggers a rollout |
| Value | SHA-256 over the **structural** content of the `<cluster>-cnf` Secret: `proxysql.cnf` with every runtime-appliable variable value replaced by a fixed placeholder, plus the raw bytes of every other Secret key (`fluent-bit.conf`) — so runtime-appliable value changes don't move it, while any structural change (or a non-`proxysql.cnf` key change) does |
| Purpose | the structural twin of `vars-applied-hash`: records what structural Secret content the StatefulSet was last successfully reconciled against. If the operator dies after writing the Secret but before updating the StatefulSet, the next reconcile sees identical old/new Secret data — without this marker the pending restart (e.g. a `fluent-bit.conf`-only change) would be silently dropped forever; the mismatch forces the restart through, surfaced as `RestartRequired: structural change pending from interrupted reconcile` |

Absent on StatefulSets created by operator versions that predate it; an
absent marker is skipped (never forces a restart on operator upgrade) and is
written on the next reconcile. Not user-configurable.

### `proxysql.com/tls-applied-hash` (operator-set, on the StatefulSet object)

| | |
|---|---|
| Set on | the StatefulSet's own `metadata.annotations` — **not** the pod template, so setting it never triggers a rollout |
| Value | deterministic SHA-256 over exactly the three Secret keys ProxySQL serves through the datadir symlinks (`ca.crt`, `tls.crt`, `tls.key`) — other keys a Secret may carry (e.g. cert-manager's `tls-combined.pem`) never reach the pods and don't move this hash |
| Purpose | records the `tls` Secret content every **ready** replica has been handshake-verified to actually serve, after `PROXYSQL RELOAD TLS`. Only advances once verification succeeds on every ready replica (or the rolling-restart fallback is committed in the same StatefulSet write) — an operator crash mid-rotation re-runs the (idempotent) `RELOAD TLS` rather than silently dropping it |

Absent when `spec.tls` is off, or on a StatefulSet from before this
annotation existed (operator upgrade) — either case is treated as "adopt
without dialing," matching the pattern for
[`structural-applied-hash`](#proxysqlcomstructural-applied-hash-operator-set-on-the-statefulset-object).
See [TLS reference](proxysqlcluster.md#tls) and the [TLS user
guide](../user-guide/tls.md#rotation) for the full rotation model. Not
user-configurable.

### `proxysql.com/tls-rotation-state` (operator-set, on the StatefulSet object)

| | |
|---|---|
| Set on | the StatefulSet's own `metadata.annotations` — never the pod template |
| Value | `<secretHash>@<RFC3339 window-start>` — the content hash being rotated to, and when the operator first attempted it |
| Purpose | anchors the bounded rotation retry window (default 2 minutes) across reconciles and operator restarts, so a transient verification failure doesn't reset the clock (which would defer the restart fallback indefinitely) and clock skew can't extend it (a future-dated start is rejected in favor of "now") |

Cleared once the window resolves — either every ready replica verifies, or
the window expires into the restart fallback. Not user-configurable.

### `proxysql.com/tls-restart` (operator-set, on the pod template)

| | |
|---|---|
| Set on | the StatefulSet pod template — a change here **does** trigger a rolling restart |
| Value | the `tls` Secret's content hash (same hash as `tls-applied-hash`) |
| Purpose | the TLS rotation engine's restart fallback: written only when the bounded rotation window (see `tls-rotation-state` above) expires with some replica still unable to complete a handshake-verified `RELOAD TLS`. Using the content hash makes the bump idempotent — one restart per rotated content, safe to recompute after a crash |

Like `cnf-checksum`, this key is **reserved**: the operator writes it after
merging `spec.podAnnotations`, so a user-supplied entry with the same key
is always overwritten. Absent until a rotation first needs the fallback
(the common case) — and sticky after that: once written, the operator
carries it forward through later restart-free rotations, because removing
it would itself change the pod template and trigger a rollout. Its
presence in `kubectl get sts <cluster> -o yaml` therefore means some past
rotation needed the fallback path, not necessarily the most recent one. See [TLS user guide — rotation](../user-guide/tls.md#rotation).

## Standard label set

Applied to every object the operator creates for a `ProxySQLCluster`
(StatefulSet, Services, Secrets, PDB, ServiceMonitor):

| Label | Value |
|---|---|
| `app.kubernetes.io/name` | `proxysql` |
| `app.kubernetes.io/instance` | `<cluster-name>` |
| `app.kubernetes.io/component` | `proxysql-cluster` |
| `app.kubernetes.io/managed-by` | `proxysql-operator` |
| `proxysql.com/cluster` | `<cluster-name>` |

### Selector labels

The subset used as the StatefulSet/Service/PDB/ServiceMonitor selector —
stable across operator upgrades by contract (selectors are immutable):

| Label | Value |
|---|---|
| `app.kubernetes.io/name` | `proxysql` |
| `app.kubernetes.io/instance` | `<cluster-name>` |
| `proxysql.com/cluster` | `<cluster-name>` |

The config reconciler discovers target pods with
`proxysql.com/cluster=<cluster-name>` alone; pod events with that label
trigger config re-reconciles. `spec.podLabels` are merged on top of the
selector labels in the pod template (selector labels win for selection).

## Finalizer: `proxysql.com/config-cleanup`

Added to every `ProxySQLConfig` on first reconcile. On deletion the operator
pushes a cleanup desired state to every ready replica — DELETEing every
managed admin table and LOAD/SAVEing each section — then releases the
finalizer. Variables are deliberately left as-is: ProxySQL has no "unset",
and blind resets would be worse than leaving the last-asserted values.
`proxysql_servers` is the one table with an exception: when the config's
peer list was operator-populated (empty `spec.proxysqlServers`), cleanup
re-pushes the auto-derived peers instead of clearing them while the target
cluster still runs more than one replica
([#42](https://github.com/ProxySQL/proxysql-on-k8s/issues/42)); an explicit
peer list is cleared like every other table.

### Wedge policy

Guiding rule: never wedge deletion when the operator cannot possibly clean
up; do hold the finalizer when pods could come back and re-expose stale
config.

| Situation at deletion time | Behavior |
|---|---|
| `proxysql.com/skip-cleanup: "true"` annotation present | Release immediately, no cleanup. |
| Referenced `ProxySQLCluster` not found | Release (nothing to clean). |
| Cluster's admin Secret not found | Release (cannot authenticate). |
| Admin Secret matches no accepted credential schema | Release without cleanup (cannot authenticate; logged). |
| Cluster exists but has **no ready pods** | **Hold** the finalizer, retry every 5s — releasing would leak config onto pods that come back. Escape hatch: the skip-cleanup annotation. |
| Cleanup reached only some replicas | Hold, retry every 5s until all ready replicas are cleaned. |

## Objects owned by a ProxySQLCluster

All carry the standard label set and a controller `ownerReference` to the
cluster (so they are garbage-collected with it; delete-protection checks
`IsControlledBy` before the operator removes anything it didn't create).

| Object | Name | Notes |
|---|---|---|
| StatefulSet | `<cluster-name>` | `podManagementPolicy: Parallel`; selector immutable after create. |
| Service (client-facing) | `<cluster-name>` | Type from `spec.service.type` (default `ClusterIP`); annotations merge, ClusterIP/ClusterIPs preserved on update. |
| Service (headless) | `<cluster-name>-headless` | `publishNotReadyAddresses: true`; StatefulSet `serviceName`. |
| Service (external) | `<cluster-name>-external` | Only when `spec.service.external.enabled: true`; deleted (if operator-owned) when disabled. Annotations merge the same way as the client-facing Service but are tracked independently. Node ports and `healthCheckNodePort` allocated by the apiserver are preserved across reconciles the same way ClusterIP is. See [ProxySQLCluster reference](proxysqlcluster.md#external-service). |
| Secret (auth) | `<cluster-name>` (only when `spec.auth.secretName` is empty) | Keys per `spec.auth.keys`; an externally referenced Secret is never owned or modified. |
| Secret (bootstrap cnf) | `<cluster-name>-cnf` | Keys `proxysql.cnf` (+ `fluent-bit.conf` when logging is enabled). A Secret because the cnf embeds passwords. |
| PodDisruptionBudget | `<cluster-name>` | Only when enabled and `replicas > 1`; deleted otherwise (if operator-owned). |
| ServiceMonitor | `<cluster-name>` | Only when metrics + serviceMonitor enabled; deleted otherwise (if operator-owned). |
| PVC (per pod) | `data-<cluster-name>-<ordinal>` | From the `data` volumeClaimTemplate; standard StatefulSet retention. |

Migration note: operator versions before v0.3.0 kept the bootstrap cnf in a
**ConfigMap** named `<cluster-name>`. Current versions delete that leftover
ConfigMap on reconcile — but only when it carries the cluster's controller
ownerReference; an unrelated user ConfigMap that merely shares the name
survives.
