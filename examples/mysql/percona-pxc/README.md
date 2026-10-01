# Percona Operator for XtraDB Cluster (PXC)

ProxySQL in front of a [Percona XtraDB Cluster](https://docs.percona.com/percona-operator-for-mysql/pxc/)
(Galera-based), routed with `mysqlGaleraHostgroups`.

Galera nodes are all equal — any of them *can* accept writes — so there is no
`read_only` flag to follow and `mysqlReplicationHostgroups` does not apply.
What does apply is `mysqlGaleraHostgroups`: ProxySQL's Galera monitor reads
each node's wsrep state and places it in one of four hostgroups.

This example declares all three nodes in the **writer hostgroup (0)** and lets
the monitor do the rest:

| hostgroup | holds |
| --- | --- |
| 0 — writer | the one elected writer (`maxWriters: 1`) |
| 2 — backup writer | the other two Synced nodes, ready to be promoted |
| 1 — reader | the backup writers, mirrored in by `writerIsAlsoReader: 2` |
| 3 — offline | any node that is not Synced, is desynced, is rejecting queries, or has fallen too far behind |

Writes therefore land on a single node. Galera certifies writes cluster-wide,
so multi-writer is *correct*, but conflicting transactions committed on
different nodes fail certification and surface as deadlock errors at `COMMIT`.
Single-writer routing avoids that. `maxWriters: 3` restores the multi-writer
behavior this example used previously, but it leaves no backup writers — so
`writerIsAlsoReader: 2` would empty the reader hostgroup and strand query rule
100. Change it to `1` in the same edit.

Because the operator only syncs the table and follows the monitor, a writer
change — election, promotion after a node leaves, a node parked offline — is
*not* treated as config drift and is never reverted by a resync.

The `monitor` user PXC ships is granted `SELECT, PROCESS, REPLICATION CLIENT,
RELOAD` among others, which covers the wsrep status the Galera check reads.

The PXC operator ships its own ProxySQL/HAProxy proxies — we disable them
in the backend CR and use the operator-managed one instead.

## What this example creates

- A 3-node `PerconaXtraDBCluster`.
- A 3-replica `ProxySQLCluster`.
- A `ProxySQLConfig` pointing at the three PXC pods directly via their
  stable per-pod DNS (so a pod restart doesn't shift our routing), with one
  `mysqlGaleraHostgroups` row wiring hostgroups 0/2/1/3 together.

## Install order

```bash
# 1. PXC Operator (pinned).
helm repo add percona https://percona.github.io/percona-helm-charts/
helm install pxc-operator percona/pxc-operator --version 1.20.0 --set watchAllNamespaces=true -n pxc-operator --create-namespace

# 2. Namespace + secrets + PerconaXtraDBCluster CR.
kubectl apply -f backend.yaml

# 3. Wait — first boot does an SST and can take several minutes.
#    Use the fully-qualified name: `pxc` is also the shortName of the operator's
#    ProxySQLCluster CRD, so the bare `pxc/` alias is ambiguous once the ProxySQL
#    operator is installed.
kubectl -n percona-pxc-demo wait perconaxtradbclusters.pxc.percona.com/cluster1 --for=jsonpath='{.status.state}'=ready --timeout=15m

# 4. ProxySQL operator — see examples/README.md.

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml
```

## Smoke test

```bash
ROOT_PW=$(kubectl -n percona-pxc-demo get secret cluster1-secrets -o jsonpath='{.data.root}' | base64 -d)
kubectl -n percona-pxc-demo run -it --rm mysql-cli --image=mysql:8.4 --restart=Never --env=MYSQL_PWD="$ROOT_PW" -- \
  mysql -h proxysql -P 6033 -uroot -e "SELECT @@wsrep_node_name, @@wsrep_cluster_status"
```

That statement is a `SELECT`, so query rule 100 routes it to the reader
hostgroup. Run it repeatedly and `wsrep_node_name` alternates between the two
**backup writers** — with `writerIsAlsoReader: 2` the elected writer is
write-only and stays out of the read pool, so you will not see all three
nodes here.

To see where writes go, and how the monitor placed each node, look at the
admin port:

```bash
RADMIN_PW=$(kubectl -n percona-pxc-demo get secret proxysql -o jsonpath='{.data.radmin-password}' | base64 -d)
kubectl -n percona-pxc-demo run -it --rm admin-cli --image=mysql:8.4 --restart=Never --env=MYSQL_PWD="$RADMIN_PW" -- \
  mysql -h proxysql -P 6032 -uradmin -e \
  "SELECT hostgroup_id, hostname, status FROM runtime_mysql_servers ORDER BY hostgroup_id, hostname"
```

One node in hostgroup 0, two in 2, the same two mirrored into 1, and nothing
in 3 while the cluster is healthy.
