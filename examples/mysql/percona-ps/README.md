# Percona Operator for MySQL Server

ProxySQL in front of the [Percona Operator for MySQL Server](https://docs.percona.com/percona-operator-for-mysql/ps/)
(PS Operator) running **async replication**: 1 primary + 1 replica. The
operator keeps `super_read_only=ON` on the replica, so ProxySQL's standard
`mysqlReplicationHostgroups` table sorts the nodes into writer/reader
hostgroups automatically — the same pattern as the
[mariadb-operator example](../mariadb-operator/).

> ⚠️ **kind-sized demo.** The backend CR sets three `unsafeFlags` to keep the
> footprint small: 2 MySQL nodes instead of the operator's safe minimum of 3,
> no orchestrator (so **no automated failover**), and no HAProxy (ProxySQL is
> the proxy layer here). For production use `mysql.size: 3` and enable
> orchestrator — the ProxySQL wiring below does not change.

> ⚠️ **Monitor password — keep two places in sync.** ProxySQL's monitor
> credentials are admin variables, which the `ProxySQLConfig` exposes as plain
> strings under `mysqlVariables` (no `secretRef` there). The PS operator
> pre-creates a `monitor` user whose password is the `monitor` key of
> `cluster1-secrets`. Both ship with the same `REPLACE-ME-monitor`
> placeholder; if you change one, change the other, or ProxySQL's monitor
> gets "Access denied", can't read each node's `read_only` flag, and parks
> every server in the reader hostgroup.

## What this example creates

- A `PerconaServerMySQL` (async, 1 primary + 1 replica), HAProxy/Router and
  orchestrator disabled, `exposePrimary` on.
- A `ProxySQLCluster` (3 replicas).
- A `ProxySQLConfig` listing both MySQL pods with a
  `mysqlReplicationHostgroups` row that splits them into hostgroup 0 (writer)
  and hostgroup 1 (reader) by their `read_only` flag.

## Install order

```bash
# 1. PS Operator (once per cluster), pinned.
helm repo add percona https://percona.github.io/percona-helm-charts/
helm install ps-operator percona/ps-operator --version 1.1.0 --set watchAllNamespaces=true -n ps-operator --create-namespace

# 2. Namespace + backend secrets + PerconaServerMySQL CR.
kubectl apply -f backend.yaml

# 3. Wait for it to become Ready (3-5 min).
kubectl -n percona-ps-demo wait perconaservermysqls.ps.percona.com/cluster1 --for=jsonpath='{.status.state}'=ready --timeout=10m

# 4. ProxySQL operator (once per cluster) — see examples/README.md.

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml
```

## Smoke test

```bash
ROOT_PW=$(kubectl -n percona-ps-demo get secret cluster1-secrets -o jsonpath='{.data.root}' | base64 -d)
kubectl -n percona-ps-demo run -it --rm mysql-cli --image=mysql:8.4 --restart=Never --env=MYSQL_PWD="$ROOT_PW" -- \
  mysql -h proxysql -P 6033 -uroot -e "SELECT @@hostname, @@read_only"
```

The `SELECT` is routed to hostgroup 1, so you should see the replica
(`cluster1-mysql-1`, `read_only=1`). Writes (and `SELECT ... FOR UPDATE`) go
to hostgroup 0 — the primary. Or run the
[sysbench loadgen](../../loadgen/sysbench.yaml) for sustained traffic.
