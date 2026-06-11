# Day-2 operations

Reading operator status correctly, finding the right log, diagnosing the
common failure modes, and touching the admin port without fighting the
operator. Companion pages: [status reference](../reference/status.md)
for every field, [annotations reference](../reference/annotations.md)
for the control annotations.

## Reading status: phase, endpoints, conditions

```bash
kubectl get pxc                     # ProxySQLCluster: REPLICAS / READY / PHASE
kubectl get pxcfg                   # ProxySQLConfig: CLUSTER / SYNCED / DRIFTED / LAST-SYNC
kubectl describe pxc proxysql       # full conditions
```

**Trust conditions over phase.** `status.phase` is a deliberately coarse
one-word projection for dashboards: `Pending` (no StatefulSet yet),
`Creating` (exists, zero ready), `Running` (all ready at current
revision), `Updating` (anything in between), `Degraded` (observed error
state). The coarseness has a sharp edge: a previously-healthy cluster
that loses *all* replicas reports `Creating`, not an error — only the
`Available=False` condition (and your monitoring) tells you it is an
outage. `Failed` is reserved and currently never set.

Cluster conditions: `Available` (full ready-replica count),
`Progressing` (rollout in flight), `Degraded` (specific error, e.g.
`AuthSecretError`), plus `ServiceMonitorReady` when a ServiceMonitor was
requested. Config conditions: `Ready`, `Progressing`, `Degraded`,
`ClusterFound`.

**`status.endpoints`** on the cluster gives ready-to-use in-cluster
`host:port` strings per enabled surface (`mysql`, `pgsql`, `admin`,
`web`, `metrics`) — point your apps and dashboards at these instead of
re-deriving Service names and default ports.

## Troubleshooting

| Symptom | Likely cause | Check / fix |
| --- | --- | --- |
| `pxcfg` `Ready=False`, reason `NoReadyReplicas` | Cluster pods not Ready yet (or all down) | `kubectl get pods -l proxysql.com/cluster=<name>`; fix the cluster first. |
| Reason `AdminSecretMissing` / `AdminSecretIncomplete` | Auth Secret absent, partial operator schema, or cnf-invalid characters in a password | Condition message names the missing keys / offending key. See [Security](./security.md#the-two-auth-schemas-and-their-validation). |
| Reason `UserSecretError` | A `passwordSecretRef` Secret or key doesn't exist | Message names the user and secret; create/fix the Secret — the watch re-syncs automatically. |
| Reason `PartialSync`, `Degraded=SyncErrors` | Some replicas unreachable or rejecting the radmin login | Read the Degraded message (per-address errors). Auth errors after rotating the auth Secret on a persistent cluster → see the [proxysql.db precedence](./clusters.md#persistence-trade-offs). |
| `ClusterFound=False` | `clusterRef` names a missing cluster (or wrong namespace — must be the same one) | `kubectl get pxc -n <ns>`. |
| `status.shunnedBackends > 0`; queries fail with no backend | ProxySQL shunned backends: connect failures, or **monitor auth failures** (no `monitor` user on the backend) | `SELECT * FROM runtime_mysql_servers` shows `SHUNNED`; the `monitor.mysql_server_connect_log` table on the admin port shows why. Fix per [the monitor user](./backends.md#the-monitor-user). |
| Pod stuck `Pending`/rejected, event `SysctlForbidden` | `tcpKeepalive` set on a pre-1.29 cluster (or sysctls not on the node's safe list) | Upgrade K8s, allow the sysctls via kubelet `--allowed-unsafe-sysctls`, or drop `spec.networking.tcpKeepalive`. |
| `kubectl delete pxcfg` hangs in `Terminating` | Finalizer cleanup can't complete: cluster exists with no ready pods, or the operator is gone | `kubectl annotate pxcfg <name> proxysql.com/skip-cleanup=true` releases it without cleanup. [Wedge policy](./configuration.md#deleting-a-proxysqlconfig). |
| `Degraded=PgsqlDisabled` on a config | pgsql servers/users/rules declared, target cluster has pgsql off | Enable `protocols.pgsql` on the cluster or remove the pgsql sections. |
| `ServiceMonitorReady=False`, reason `CRDNotInstalledOrFailed` | Prometheus Operator CRDs missing | Install prometheus-operator or disable `spec.metrics.serviceMonitor`. Non-fatal — everything else reconciles. |
| Admin login fails: "User 'admin' can only connect locally" | Using `admin` over the network | Use `radmin` for any remote admin connection. |
| New spec field silently dropped on apply | CRDs older than the operator | Re-apply the CRDs ([upgrade notes](./installation.md#crd-handling)). |

## Where logs live

| What | Where |
| --- | --- |
| Operator decisions (reconciles, sync errors, drift detections, finalizer activity) | `kubectl -n proxysql-system logs deploy/proxysql-operator` |
| ProxySQL itself (startup, monitor, shunning) | `kubectl logs <pod> -c proxysql` |
| Query log shipper (when `spec.logging.enabled`) | `kubectl logs <pod> -c fluent-bit` — with the default `stdout` sink this *is* the query log stream |
| Raw eventslog files | `/var/log/proxysql/queries*` inside the pod (logging enabled only) |

Drift events are logged by the operator at info level
(`"runtime drift detected"` with the diff), so out-of-band tampering
leaves an audit trail.

## Metrics

- **ProxySQL pods:** Prometheus metrics at `:6070/metrics` per pod
  (ProxySQL's REST API exporter; on by default, `spec.metrics`). Exposed
  through the regular Service; `spec.metrics.serviceMonitor.enabled`
  creates a ServiceMonitor for Prometheus Operator setups. Walkthrough
  in [Tutorial 06 — monitoring](../tutorials/06-monitoring.md).
- **The operator:** standard controller-runtime metrics, HTTPS on
  `:8443` by default with authn/authz filtering; chart values under
  `metrics.*` (see the [Helm values reference](../reference/helm-values.md)).

## Drift self-healing: what to expect

The operator is level-based with bounded staleness:

- **Spec change / Secret rotation / cluster change:** re-push begins on
  the next reconcile — effectively immediately (watches fire).
- **Pod restart or recreation:** the pod watch re-pushes config as soon
  as the pod is Ready; a recreated pod (new IP) also busts the sync
  fingerprint. You should see single-digit seconds, not minutes.
- **Out-of-band mutation of managed admin tables:** healed within one
  resync interval (default **2 minutes**; operator flag
  `--config-resync-interval`, chart value `configResyncInterval`). The
  resync reads runtime state back and re-pushes only drifted replicas.
- **A 30-second safety requeue** runs between resyncs as a cheap
  no-op check.

What is *not* self-healed: variables you set out-of-band that the spec
doesn't mention (the operator only writes declared variables), tables
outside the managed set, and anything on a `ProxySQLConfig` that was
deleted with `skip-cleanup`.

## Manual admin-port access

Sometimes you need to look inside ProxySQL. Do it read-only, as
`radmin`:

```bash
NS=default; CLUSTER=proxysql
RADMIN=$(kubectl -n $NS get secret $CLUSTER \
  -o jsonpath='{.data.radmin-password}' | base64 -d)

kubectl -n $NS run admin-client --rm -it --restart=Never \
  --image=mysql:8.0 --env=MYSQL_PWD="$RADMIN" -- \
  mysql -h $CLUSTER -P6032 -uradmin
```

Useful inspection queries:

```sql
SELECT hostgroup_id, hostname, port, status FROM runtime_mysql_servers;
SELECT rule_id, match_digest, destination_hostgroup, apply
  FROM runtime_mysql_query_rules ORDER BY rule_id;
SELECT * FROM stats_mysql_connection_pool;
SELECT * FROM monitor.mysql_server_connect_log
  ORDER BY time_start_us DESC LIMIT 10;
```

**The danger of out-of-band writes:** any write to a managed table
(`mysql_servers`, `mysql_users`, `mysql_query_rules`,
`mysql_replication_hostgroups`, `mysql_hostgroup_attributes`, the
`pgsql_*` equivalents, `proxysql_servers`) will be **reverted** at the
next push or within the resync interval — each sync replaces those
tables wholesale. Worse, a manual "fix" can mask a real problem until
the resync removes it, typically mid-incident. If runtime needs to
change, change the `ProxySQLConfig` (or its referenced Secrets) and let
the operator converge. The one sanctioned exception is reading; for
emergency writes, expect a ≤2-minute lifespan and follow up with a CR
change.

## See also

- [Clusters](./clusters.md) — rolling updates, scaling semantics.
- [Configuration](./configuration.md) — sync model, deletion semantics.
- [Tutorial 04 — high availability](../tutorials/04-high-availability.md).
