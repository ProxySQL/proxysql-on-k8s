# Percona Operator for XtraDB Cluster (PXC)

ProxySQL in front of a [Percona XtraDB Cluster](https://docs.percona.com/percona-operator-for-mysql/pxc/)
(Galera-based, multi-writer).

Galera nodes are all equal — any of them accepts writes. The standard
ProxySQL pattern is:

- Put all three nodes in **hostgroup 0** so writes load-balance.
- Mirror them into **hostgroup 1** for SELECTs (lets you size the read
  pool separately if you ever want to add weight-based steering).
- Skip `mysql_replication_hostgroups` entirely; Galera maintains a single
  state, so there's no `read_only` flag to monitor.

The PXC operator ships its own ProxySQL/HAProxy proxies — we disable them
in the backend CR and use the operator-managed one instead.

## What this example creates

- A 3-node `PerconaXtraDBCluster`.
- A 3-replica `ProxySQLCluster`.
- A `ProxySQLConfig` pointing at the three PXC pods directly via their
  stable per-pod DNS (so a pod restart doesn't shift our routing).

## Install order

```bash
# 1. PXC Operator.
helm repo add percona https://percona.github.io/percona-helm-charts/
helm install pxc-operator percona/pxc-operator -n pxc-operator --create-namespace

# 2. Backend.
kubectl create namespace percona-pxc-demo
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
kubectl -n percona-pxc-demo run -it --rm mysql-cli --image=mysql:8.0 --restart=Never -- \
  mysql -h proxysql -P 6033 -uroot -p"$ROOT_PW" -e "SELECT @@wsrep_node_name, @@wsrep_cluster_status"
```

Run repeatedly — you should see `wsrep_node_name` change as ProxySQL
load-balances across the three nodes.
