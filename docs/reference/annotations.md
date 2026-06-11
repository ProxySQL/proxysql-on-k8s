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
| Purpose | any change to `proxysql.cnf` *or* `fluent-bit.conf` changes the annotation, which changes the pod template, which triggers a StatefulSet rolling restart |

This key is **reserved**: the operator writes it *after* merging
`spec.podAnnotations`, so a user-supplied entry with the same key is always
overwritten and can never clobber the rollout trigger. Don't set it — it
carries no user-configurable meaning.

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
pushes an **empty desired state** to every ready replica — which DELETEs
every managed admin table and LOAD/SAVEs each section — then releases the
finalizer. Variables are deliberately left as-is: ProxySQL has no "unset",
and blind resets would be worse than leaving the last-asserted values. Note
that the cleanup push currently also clears `proxysql_servers` (the deletion
path does not auto-populate peers —
[#42](https://github.com/ProxySQL/proxysql-on-k8s/issues/42)).

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
| Service (client-facing) | `<cluster-name>` | ClusterIP; annotations merge, ClusterIP/ClusterIPs preserved on update. |
| Service (headless) | `<cluster-name>-headless` | `publishNotReadyAddresses: true`; StatefulSet `serviceName`. |
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
