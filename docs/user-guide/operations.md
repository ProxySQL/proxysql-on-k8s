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
| Reason `PartialSync`, `Degraded=SyncErrors` | Some replicas unreachable or rejecting the radmin login | Read the Degraded message (per-address errors). Auth errors right after rotating the auth Secret are transient while pods roll; if they persist on a PVC-backed cluster, see the [cnf/proxysql.db merge rules](./clusters.md#persistence-trade-offs). |
| `Degraded=True`, reason `RuntimeApplyError` | A `spec.variables` runtime push to a replica's admin port failed — admin port unreachable, or bad radmin credential | The Degraded message names the failing replica; check its admin connectivity/credentials. The operator retries on requeue; StatefulSet updates are **not** blocked meanwhile — other pending template/replica changes still apply. |
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

## What restarts pods, what doesn't

Two spec fields push ProxySQL variables, and they don't restart pods the
same way: `ProxySQLCluster.spec.variables` (bootstrap cnf, this section) and
`ProxySQLConfig.spec.{admin,mysql,pgsql}Variables` (always runtime-only, see
[Configuration](./configuration.md#hostgroup-attributes-and-variables)) —
don't confuse the two.

**Restart-free by default.** Editing an *existing* key's value under
`spec.variables.{admin,mysql,pgsql}` on a `ProxySQLCluster` does not roll
your pods. The operator writes the updated `<cluster>-cnf` Secret first
(so a pod that does restart, for any reason, always boots correct), then
pushes the changed variables to every currently **Ready** replica over the
admin port (`UPDATE global_variables` + `LOAD ... TO RUNTIME` +
`SAVE ... TO DISK`), and reads `runtime_global_variables` back to confirm
the value actually took. Watch it land:

```bash
kubectl edit pxc proxysql   # change spec.variables.mysql.mysql-max_connections
kubectl get pxc proxysql -o jsonpath='{.status.conditions[?(@.type=="Progressing")]}'
# reason: RuntimeApplied, message: "RuntimeApplied: mysql-max_connections"
```

No pod restarts; `kubectl get pods` shows unchanged `AGE`/`RESTARTS`.

**If a push fails.** An unreachable admin port or bad radmin credential on
one replica surfaces as `Degraded=True`, reason `RuntimeApplyError`,
naming the failing replica — the operator retries on the next requeue and
does not wedge: the StatefulSet is still ensured and every other pending
change still applies.

**Automatic fallback to a restart.** Not every ProxySQL variable is
runtime-settable — some only take effect on the next start (e.g.
`mysql-threads`). The operator doesn't maintain a list of which are which;
it tries the runtime apply, reads back `runtime_global_variables`, and if
the value didn't actually change, it falls back to a rolling restart on
its own. You'll see this in the `Progressing` condition:

```
reason: Rolling, message: "RestartRequired: mysql-threads (runtime read-back mismatch)"
```

**Removing a variable key always restarts, by design.** ProxySQL has no
"unset" for a global variable — a runtime `UPDATE` can only *set* a value,
never remove one. So deleting a key from `spec.variables` can't be applied
at runtime without silently keeping the old value; the operator forces a
rolling restart instead. With persistence disabled that restart
re-bootstraps from the new cnf's variable set as a whole; with persistence
**enabled (the default)** the removed value survives the restart in
`proxysql.db` — the `--reload` merge re-applies cnf lines over the db but
never deletes db entries absent from the cnf (see the persistence note
below). This is the same reason
`ProxySQLConfig`'s variable maps document "removing a key does not reset
the variable" — different mechanism, same underlying ProxySQL limitation.

**Persistence: what a restart actually reloads.** The ProxySQL container
starts with `--reload`, so on a persistence-enabled cluster (the
**default**, `persistence.enabled: true`) every start merges the bootstrap
cnf **over** the existing `proxysql.db` on the PVC: a variable present in
both takes the **cnf value**, a variable present only in the db keeps its
db value, and the merged result is saved back to disk. A restart therefore
*does* re-read the updated cnf's variables — variables **added** to the
cnf land after the rollout, and replicas that missed a runtime push (e.g.
not Ready at push time) converge to the cnf on their next start. The one
remaining gap is **removal**: a variable deleted from the cnf keeps its
old value in `proxysql.db` (the merge never deletes db-only entries) —
after removing a `spec.variables` key on a PVC-backed cluster, verify on
the admin port and, if needed, set the intended value explicitly (via
`spec.variables` or `ProxySQLConfig`) or recreate the PVC. Upstream
documents the `--reload` merge as best-effort ("no guarantee … validate
that the merge was as expected"), so treat the admin port as the source of
truth for anything critical. Persistence-disabled (`emptyDir`) clusters
re-bootstrap from the cnf alone on every pod start and have neither
behavior to think about.

**What always restarts, unconditionally:** listening ports/interfaces,
`replicas`, admin/radmin credential rotation, and toggling the logging
sidecar — anything that isn't a `spec.variables` value change. See the
[full breakdown](../reference/proxysqlcluster.md#configuration-changes-runtime-vs-restart).

**The monitor-credential exception.** Admin/radmin credential rotation
always restarts (the `admin_credentials` cnf line changes). The `monitor`
account is different: its password is an ordinary `mysql_variables` /
`pgsql_variables` line, not part of `admin_credentials`, so rotating it
through `spec.auth`'s monitor key goes through the same restart-free
runtime-apply path as any other variable change — you can rotate the
monitor password with zero pod restarts.

**Precedence when both mechanisms touch the same variable.** If a key
appears in both `ProxySQLCluster.spec.variables` and a
`ProxySQLConfig.spec.*Variables` map targeting that cluster,
`ProxySQLConfig`'s sync runs after the cluster-level bootstrap variables on
every reconcile pass and wins — the same convergence order you'd see after
a fresh restart (cnf boots first, `ProxySQLConfig` syncs after). Don't rely
on this for anything subtle: it's simpler to pick one mechanism per
variable.

**Two operational gaps worth knowing:**

- **Zero ready replicas.** If no replica is Ready when a `spec.variables`
  change lands, nothing is pushed anywhere — there's no pod to dial. The
  cnf Secret is already updated, so a pod bootstraps the intended values
  when it comes up: a fresh datadir reads the cnf outright, and a
  PVC-backed pod restarting into an existing `proxysql.db` merges the
  updated cnf over it via `--reload` (removed keys excepted — see the
  persistence note above). If in doubt once replicas are Ready, verify on
  the admin port or re-touch the value under `spec.variables`.
- **An in-place container restart racing Secret propagation.** Kubelet
  syncs Secret volumes with a small lag (typically under a minute). If the
  proxysql container crashes and restarts in place inside that window —
  after a runtime apply succeeded but before the updated cnf reached the
  pod's mount — `--reload` re-applies the *old* cnf value over the newer
  one saved in `proxysql.db`, and the operator's markers consider the
  change applied. The divergence is silent until the next `spec.variables`
  change or restart. If a value looks stale after a crash that closely
  followed an edit, re-touch it under `spec.variables`.
- **A Not-Ready replica at push time.** The runtime push only dials
  **Ready** pods. A replica that's transiently Not-Ready (not restarting,
  just failing readiness) at the moment of the push can miss that
  variable's runtime apply and keep serving the old value until the next
  `spec.variables` change or a restart. On a multi-replica cluster,
  ProxySQL Cluster sync (`cluster_*` settings, replicas > 1) mitigates
  this for variables that also propagate via cluster sync; it does not
  cover every variable. If you need certainty a change reached every
  replica, check `runtime_global_variables` yourself (see
  [manual admin-port access](#manual-admin-port-access) below) rather than
  trusting the `Progressing` message alone — it reflects only the
  replicas that were Ready at push time.

## Rotating the monitor credential

The [monitor-credential exception](#what-restarts-pods-what-doesnt) above
means a `spec.auth` monitor-password rotation is restart-free: the operator
re-renders the `<cluster>-cnf` Secret, then pushes `mysql-monitor_password`
(and `pgsql-monitor_password`, when `protocols.pgsql` is on) to every
**Ready** replica over the admin port with no pod restart. This section is
the full backend-to-ProxySQL rotation runbook, MySQL and PostgreSQL, using
each engine's own credential-rotation primitive.

**One password drives both protocols.** The auth Secret has a single
`monitor-password` key (default; overridable via `spec.auth.keys.monitorPassword`),
and the operator renders that one value into *both* `mysql-monitor_password`
and `pgsql-monitor_password` when both protocols are enabled on the same
cluster (`operator/internal/controller/builders/proxysql_cnf.go`) — there's
no separate key per protocol. If a single `ProxySQLCluster` fronts both
MySQL and PostgreSQL backends, plan the two backends' monitor-user rotations
around the same Secret update; you can't dual-password-rotate one protocol's
backends while leaving the other on the old password.

### MySQL: dual-password rotation

MySQL 8.0.14+ lets the `monitor` account hold two valid passwords at once,
so ProxySQL keeps authenticating with the old one for as long as it takes
the new one to reach every replica — no window where the monitor is locked
out.

1. **On the backend primary**, add the new password without invalidating
   the current one:

   ```sql
   ALTER USER 'monitor'@'%' IDENTIFIED BY '<new-password>' RETAIN CURRENT PASSWORD;
   ```

   Both passwords authenticate from this point on. (Generic MySQL behavior,
   not operator-specific — if your backends aren't a single replicated
   topology the primary's `ALTER USER` replicates to, run it against every
   node independently.)

2. **Update the operator-referenced auth Secret's monitor key** to the same
   new password:

   ```bash
   NS=default; CLUSTER=proxysql
   kubectl -n $NS patch secret $CLUSTER --type=merge \
     -p="{\"data\":{\"monitor-password\":\"$(printf '%s' '<new-password>' | base64 -w0)\"}}"
   ```

   The Secret watch fires the next reconcile immediately (see [drift
   self-healing](#drift-self-healing-what-to-expect)): the `<cluster>-cnf`
   Secret is re-rendered first, then the new password is pushed to every
   Ready replica at runtime — no restart, no manual `LOAD`/`SAVE`.

3. **Verify before discarding the old password.** Two independent signals,
   both worth checking — don't rely on either alone:

   - The `Progressing` condition confirms the runtime push landed:

     ```bash
     kubectl get pxc $CLUSTER -o jsonpath='{.status.conditions[?(@.type=="Progressing")]}'
     # reason: RuntimeApplied, message: "RuntimeApplied: mysql-monitor_password"
     ```

     This only reflects replicas that were **Ready** at push time (the
     [Not-Ready-replica gap](#what-restarts-pods-what-doesnt) applies here
     too) — if a replica was down or failing readiness during the push, it
     didn't get the new password and this message won't tell you that.
   - On the admin port (see [manual admin-port
     access](#manual-admin-port-access)), confirm every replica actually has
     the new value and is connecting successfully:

     ```sql
     SELECT variable_name, variable_value FROM runtime_global_variables
       WHERE variable_name='mysql-monitor_password';
     SELECT hostname, port, time_start_us, connect_success_time_us, connect_error
       FROM monitor.mysql_server_connect_log ORDER BY time_start_us DESC LIMIT 10;
     SELECT hostname, port, time_start_us, ping_success_time_us, ping_error
       FROM monitor.mysql_server_ping_log ORDER BY time_start_us DESC LIMIT 10;
     ```

     `runtime_global_variables` returns `mysql-monitor_password` in
     **plaintext, not masked** — this is a known upstream ProxySQL
     characteristic, not something the operator adds; treat admin-port
     access accordingly (it's already restricted to the operator and DBA
     tooling per the [network exposure
     surface](./security.md#network-exposure-surface)). A `connect_error`/
     `ping_error` of `NULL` on rows timestamped after the rotation is your
     per-replica confirmation.

   Repeat the admin-port check against **every** replica — `radmin` on
   6032 only shows you the one pod you connected to, and the Not-Ready-replica
   gap means a healthy-looking `Progressing` message can still leave one pod
   behind.

4. **Once every replica confirms**, discard the old password on the
   backend:

   ```sql
   ALTER USER 'monitor'@'%' DISCARD OLD PASSWORD;
   ```

   Discarding before every replica has the new password locks out whichever
   replica didn't get it — the entire point of the dual-password window is
   to not have to race that.

### PostgreSQL: no dual passwords

Vanilla PostgreSQL has no equivalent to MySQL's `RETAIN CURRENT PASSWORD`:
`ALTER ROLE ... PASSWORD` replaces the password atomically, and `VALID
UNTIL` sets an expiry timestamp, not a second valid password. There is an
unavoidable moment where the old password stops working before every
ProxySQL replica has the new one — the goal is to make that window as short
as possible, not to eliminate it.

1. **Prepare the new password** ahead of time so both commands below can
   fire back-to-back with no thinking time in between.
2. **Run the backend change and the Secret update in quick succession**:

   ```sql
   -- backend primary
   ALTER ROLE monitor WITH PASSWORD '<new-password>';
   ```

   ```bash
   # immediately after, same terminal session
   NS=default; CLUSTER=proxysql
   kubectl -n $NS patch secret $CLUSTER --type=merge \
     -p="{\"data\":{\"monitor-password\":\"$(printf '%s' '<new-password>' | base64 -w0)\"}}"
   ```

   The shorter the gap, the fewer failed probes. ProxySQL's monitor module
   retries connect/ping checks on its own interval
   (`pgsql-monitor_connect_interval` / `pgsql-monitor_ping_interval`); a
   handful of failed probes during the gap is expected and, per [the
   monitor user](./backends.md#the-monitor-user), it takes sustained
   failures — not one blip — before ProxySQL shuns an otherwise-healthy
   backend. Still, watch `status.shunnedBackends` on the `ProxySQLConfig`
   if the gap runs longer than expected.
3. **Verify** the same way as MySQL, using the PostgreSQL monitor log
   tables:

   ```sql
   SELECT variable_name, variable_value FROM runtime_global_variables
     WHERE variable_name='pgsql-monitor_password';
   SELECT hostname, port, time_start_us, connect_success_time_us, connect_error
     FROM monitor.pgsql_server_connect_log ORDER BY time_start_us DESC LIMIT 10;
   SELECT hostname, port, time_start_us, ping_success_time_us, ping_error
     FROM monitor.pgsql_server_ping_log ORDER BY time_start_us DESC LIMIT 10;
   ```

   `connect_error`/`ping_error` clearing up (`NULL`) on rows timestamped
   after the coordinated update, on every replica, is confirmation the
   rotation reached them. There's no step 4: with no second password to
   discard, this is the whole rotation.

A **Not-Ready replica at push time** is worth calling out specifically
here: without a dual-password grace window, that replica keeps trying the
*old* password against a backend that no longer accepts it until the next
reconcile reaches it — a longer, but still self-recovering, version of the
same gap [documented above](#what-restarts-pods-what-doesnt). It clears up
the moment that replica becomes Ready and picks up the new password from
the next resync; nothing to do but be aware it can outlast the "quick
succession" window if a replica is down for a while.

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
