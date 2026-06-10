# Oracle MySQL Operator (InnoDB Cluster)

ProxySQL in front of [Oracle's MySQL Operator for Kubernetes](https://github.com/mysql/mysql-operator).

**ProxySQL replaces MySQL Router here.** The InnoDBCluster runs with
`router.instances: 0` — ProxySQL talks straight to the MySQL instance via the
operator's `mycluster-instances` headless Service and provides the access
layer (connection multiplexing, query rules, stats) that Router would
otherwise be.

> **kind-sized demo.** One server instance keeps this light enough for a
> laptop kind cluster. With `instances: 1` there's no Group Replication
> failover, so everything sits in hostgroup 0. For real HA bump `instances`
> to 3 and add the secondaries to hostgroup 1 — and note that ProxySQL-native
> tracking of GR topology (`mysql_group_replication_hostgroups`) is on the
> post-v2 CRD roadmap; until then, re-point hostgroup 0 manually or via the
> operator's primary labels on failover.

## What this example creates

- A 1-instance `InnoDBCluster` (`mysql.oracle.com/v2`) with **0** Router pods
  and self-signed TLS.
- A `ProxySQLCluster` (3 replicas).
- A `ProxySQLConfig` with hostgroup 0 → `mycluster-0` over TLS (MySQL 8.4/9.x
  accounts use `caching_sha2_password`, which wants a secure channel — note
  the `useSSL: true` on the server entry).

## Install order

```bash
# 1. Oracle MySQL Operator (once per cluster), pinned.
helm repo add mysql-operator https://mysql.github.io/mysql-operator/
helm install mysql-operator mysql-operator/mysql-operator --version 2.2.8 -n mysql-operator --create-namespace

# 2. Namespace + root secret + InnoDBCluster.
kubectl apply -f backend.yaml

# 3. Wait for the InnoDBCluster to reach status ONLINE (can take ~3 min).
kubectl -n oracle-mysql-demo wait innodbcluster/mycluster --for=jsonpath='{.status.cluster.status}'=ONLINE --timeout=10m

# 4. ProxySQL operator (once per cluster) — see examples/README.md.

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml

# 6. Watch the config sync to all replicas.
kubectl -n oracle-mysql-demo get proxysqlconfig pxcfg -w
```

## Smoke test

```bash
ROOT_PW=$(kubectl -n oracle-mysql-demo get secret mycluster-secret -o jsonpath='{.data.rootPassword}' | base64 -d)
kubectl -n oracle-mysql-demo run -it --rm mysql-cli --image=mysql:8.4 --restart=Never --env=MYSQL_PWD="$ROOT_PW" -- \
  mysql -h proxysql -P 6033 -uroot -e "SELECT @@hostname, @@version"
```

You should get `mycluster-0` and the MySQL server version (9.x with operator
chart 2.2.8). Or run the [sysbench loadgen](../../loadgen/sysbench.yaml) with
`HOST=proxysql.oracle-mysql-demo.svc` for sustained traffic.
