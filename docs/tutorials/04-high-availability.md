# Tutorial 4 — High availability

**What you'll learn**

- Scaling a `ProxySQLCluster` to 3 replicas, and what changes when you do
  (PodDisruptionBudget, peer list)
- How config reaches every replica: write-to-all and `syncedReplicas`
- What ProxySQL Cluster sync adds on top
- Killing a pod and watching the operator re-converge it
- How out-of-band drift is detected and healed (`driftedReplicas`)
- Where `read_only`-based failover fits (`mysqlReplicationHostgroups`)

**Prerequisites**

- [Tutorial 2](02-query-routing.md) completed, with the `proxysql-tutorial`
  namespace still around. ([Tutorial 3](03-postgresql.md) is independent —
  skipping it is fine.)

## 1. Scale to three replicas

One proxy pod is a single point of failure. Scale the cluster:

```sh
kubectl -n proxysql-tutorial patch proxysqlcluster proxysql --type=merge -p '{"spec":{"replicas":3}}'
kubectl -n proxysql-tutorial rollout status statefulset/proxysql --timeout=300s
kubectl -n proxysql-tutorial get pxc,pxcfg,pdb
```

```
NAME                                    REPLICAS   READY   PHASE     AGE
proxysqlcluster.proxysql.com/proxysql   3          3       Running   4m40s

NAME                                   CLUSTER    SYNCED   DRIFTED   LAST-SYNC   AGE
proxysqlconfig.proxysql.com/proxysql   proxysql   3                  8s          4m15s

NAME                                  MIN AVAILABLE   MAX UNAVAILABLE   ALLOWED DISRUPTIONS   AGE
poddisruptionbudget.policy/proxysql   2               N/A               1                     19s
```

(If `SYNCED` still reads 2 the moment the rollout finishes, give it a few
seconds — the last pod is synced as soon as its readiness flips.) Three
things happened:

- The StatefulSet grew to `proxysql-0/1/2`. You may notice existing pods
  restart once: going above one replica changes the bootstrap config (the
  peer list below), and bootstrap/structural config changes roll the
  StatefulSet (variable value edits are applied at runtime instead).
- A **PodDisruptionBudget** appeared, defaulting to `minAvailable: 2`
  (`replicas - 1`) — voluntary disruptions like node drains can never take
  the cluster below two ready proxies. It's omitted entirely at
  `replicas: 1`, which is why you didn't see it in tutorial 1. Tune it via
  `spec.podDisruptionBudget` ([reference](../reference/proxysqlcluster.md)).
- `SYNCED` went to **3**: the operator pushed the existing `ProxySQLConfig`
  to each new replica as it became ready, without you touching the config.

## 2. Write-to-all, and what cluster sync adds

How did all three pods get the same config? **The operator writes to all of
them.** On every sync it connects to each ready replica's admin port and
applies the full desired state — `status.syncedReplicas` counts exactly how
many replicas took the latest config. There is no "primary" proxy that the
others copy from; the Kubernetes API is the source of truth, and the
operator is the distributor. (Rationale in
[architecture.md](../architecture.md#why-write-to-all-instead-of-letting-proxysql-cluster-sync-handle-it).)

Independently of that, ProxySQL has a *native* clustering mechanism: when
`replicas > 1`, the operator seeds each pod's bootstrap config with the peer
list (every pod's stable headless-DNS name on admin port 6032), so ProxySQL's
own cluster module can diff-and-pull config between peers as a
belt-and-braces backup between operator syncs.

> [!NOTE]
> You don't have to manage the peer list yourself. When a `ProxySQLConfig`
> omits `spec.proxysqlServers` and the cluster runs more than one replica,
> the operator auto-populates `proxysql_servers` on every sync from the
> cluster's stable per-pod DNS names (admin port, weight 0, comment
> `operator-populated from ProxySQLCluster pods`) — so applying a config
> never wipes the cnf-seeded peers, and native cluster sync stays active
> alongside the operator's direct writes. An explicit `proxysqlServers`
> list is passed through unchanged; at `replicas: 1` the peer table stays
> empty (there are no peers). *Deleting* a `ProxySQLConfig` also preserves
> the auto-populated peers: its cleanup clears every managed table but
> re-pushes the derived peer list while the cluster still runs more than
> one replica
> ([#42](https://github.com/ProxySQL/proxysql-on-k8s/issues/42)).
> Details in the
> [proxysqlServers reference](../reference/proxysqlconfig.md#proxysqlservers).

## 3. Kill a pod and watch it re-converge

Delete a pod. The StatefulSet recreates it (new IP, empty runtime tables) —
and the operator watches pod readiness, so the replacement gets the config
pushed the moment it's Ready, not on the next timer tick:

```sh
kubectl -n proxysql-tutorial delete pod proxysql-1
kubectl -n proxysql-tutorial wait --for=condition=Ready pod/proxysql-1 --timeout=120s
```

Ask the *recreated pod directly* (via its stable headless-DNS name) what it
is running:

```sh
RADMIN_PW="$(kubectl -n proxysql-tutorial get secret proxysql -o jsonpath='{.data.radmin-password}' | base64 -d)"
kubectl -n proxysql-tutorial run admin-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD="$RADMIN_PW" -- \
  mysql -h proxysql-1.proxysql-headless -P6032 -uradmin -e "SELECT hostgroup_id, hostname, status FROM runtime_mysql_servers"
kubectl -n proxysql-tutorial get pxcfg proxysql
```

```
hostgroup_id	hostname	status
0	mysql.proxysql-tutorial.svc.cluster.local	ONLINE
10	mysql-reader.proxysql-tutorial.svc.cluster.local	ONLINE
pod "admin-client" deleted
NAME       CLUSTER    SYNCED   DRIFTED   LAST-SYNC   AGE
proxysql   proxysql   3                  12s         7m46s
```

In the validated run the replacement pod had its full config back roughly
ten seconds after becoming Ready. During the gap, clients were unaffected:
the Service only routes to ready pods, and the other two replicas kept
serving.

## 4. Out-of-band drift: detection and self-heal

What if someone logs into one replica's admin port and changes things
behind the operator's back? Simulate it — wipe the server list on
`proxysql-2` only:

```sh
kubectl -n proxysql-tutorial run admin-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD="$RADMIN_PW" -- \
  mysql -h proxysql-2.proxysql-headless -P6032 -uradmin -e "DELETE FROM mysql_servers; LOAD MYSQL SERVERS TO RUNTIME; SELECT COUNT(*) AS servers_now FROM runtime_mysql_servers"
```

```
servers_now
0
```

Nothing about the CRs changed, so no event wakes the operator — this is what
the periodic **resync interval** (default 2 minutes; tunable via the chart's
`configResyncInterval`) exists for. When it elapses, the operator *reads
runtime state back* from every replica, records any divergence in
`status.driftedReplicas`, and re-pushes **only the drifted replicas** — a
converged cluster sees read-only checks, not constant rewrites. Watch it
heal:

```sh
kubectl -n proxysql-tutorial run admin-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD="$RADMIN_PW" -- \
  mysql -h proxysql-2.proxysql-headless -P6032 -uradmin -e "SELECT COUNT(*) AS servers_now FROM runtime_mysql_servers"
```

In the validated run the count was back to `2` after ~100 seconds. Because
detection and re-push happen inside the same reconcile, `driftedReplicas`
usually reads `0` again by the time you poll it — treat *non-zero* values as
"a replica drifted and could not be brought back yet", which is worth
alerting on ([tutorial 6](06-monitoring.md)). The timestamps tell the story:

```sh
kubectl -n proxysql-tutorial get pxcfg proxysql -o yaml | sed -n '/^status:/,$p'
```

```yaml
status:
  conditions:
  - message: ProxySQLCluster resolved
    reason: Found
    status: "True"
    type: ClusterFound
  - message: config applied to 3/3 replicas
    reason: Synced
    status: "True"
    type: Ready
  - message: ""
    reason: Steady
    status: "False"
    type: Progressing
  lastAppliedHash: efbe4fe4637f285954e2cd7bf9e4dd232b430dcd1a94880dec01613aeaf390dd
  lastRuntimeCheckTime: "2026-06-11T09:57:30Z"
  lastSyncTime: "2026-06-11T09:57:30Z"
  observedGeneration: 3
  syncedReplicas: 3
```

`lastRuntimeCheckTime` advancing while `syncedReplicas` stays full is the
steady-state heartbeat: the operator keeps *verifying*, and only rewrites
what actually diverged.

## 5. Backend failover: replication hostgroups

So far HA covered the *proxy* layer. For the *backend* layer, ProxySQL can
react to primary failover on its own — declare a writer/reader hostgroup
pair, and ProxySQL continuously checks each backend's `read_only` flag,
moving servers between the two hostgroups when roles change:

```yaml
spec:
  mysqlReplicationHostgroups:
    - writerHostgroup: 0
      readerHostgroup: 10
      checkType: read_only
```

A backend reporting `read_only=0` lands in hostgroup 0 (writes), 
`read_only=1` in hostgroup 10 (reads) — so when your database operator
promotes a replica, traffic follows within seconds, with no Kubernetes-level
change at all. This needs a working `monitor` user on the backends (which
our demo pods don't have — it's disabled in this namespace), so it's not run
here. The full stance per backend operator — who owns failover, which
`checkType` to use, monitor-user setup — is in
[user-guide/backends.md](../user-guide/backends.md).

## Clean up

**Continuing to [tutorial 5](05-query-logging.md)? Keep the namespace.**

```sh
kubectl delete namespace proxysql-tutorial
```

## Next

[Tutorial 5 — Query logging](05-query-logging.md): ship every query
ProxySQL serves to stdout (or S3/HTTP) with the Fluent Bit sidecar.
