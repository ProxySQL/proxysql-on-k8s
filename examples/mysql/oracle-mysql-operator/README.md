# Oracle MySQL Operator (InnoDB Cluster)

ProxySQL in front of [Oracle's MySQL Operator for Kubernetes](https://github.com/mysql/mysql-operator).

## What this example creates

- An `InnoDBCluster` (`mysql.oracle.com/v2`) with 3 instances + 1 MySQL Router pod.
- A `ProxySQLCluster` (3 replicas).
- A `ProxySQLConfig` that targets the Router's R/W (6446) and R/O (6447) ports
  as hostgroup 0 / 1 respectively. Letting Router decide which instance is
  primary keeps the example simple; ProxySQL adds connection multiplexing,
  query rules, and statistics on top.

> If you need ProxySQL itself to monitor group-replication health (rather
> than relying on the Router), the CRD field for `mysql_group_replication_hostgroups`
> is on the post-v2 roadmap. Until then, this example is the simplest reliable
> wiring.

## Install order

```bash
# 1. Oracle MySQL Operator (once per cluster).
helm repo add mysql-operator https://mysql.github.io/mysql-operator/
helm install mysql-operator mysql-operator/mysql-operator -n mysql-operator --create-namespace

# 2. Namespace + backend root secret + InnoDBCluster.
kubectl create namespace oracle-mysql-demo
kubectl -n oracle-mysql-demo create secret generic mycluster-secret \
  --from-literal=rootUser=root \
  --from-literal=rootHost=% \
  --from-literal=rootPassword="$(openssl rand -hex 16)"
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
kubectl -n oracle-mysql-demo exec -it deploy/proxysql -- \
  mysql -h 127.0.0.1 -P 6033 -uapp -p"$(kubectl -n oracle-mysql-demo get secret mycluster-secret -o jsonpath='{.data.rootPassword}' | base64 -d)" \
  -e "SELECT @@hostname, @@read_only"
```

Run it twice — you should see different hostnames as ProxySQL load-balances
through Router. Or run the [sysbench loadgen](../../loadgen/sysbench.yaml) with
`HOST=proxysql.oracle-mysql-demo.svc` for sustained traffic.
