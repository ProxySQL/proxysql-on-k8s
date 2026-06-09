# Percona Operator for MySQL Server

ProxySQL in front of the [Percona Operator for MySQL Server](https://docs.percona.com/percona-operator-for-mysql/ps/)
(PS Operator), running MySQL Group Replication. With `exposePrimary` enabled
the operator publishes a `cluster1-mysql-primary` Service that always points
at the current GR primary and is re-pointed on failover — so ProxySQL needs
just one `mysql_servers` row to always reach the writer.

> **Reads:** the PS operator does **not** create a `-mysql-replicas` Service in
> the HAProxy-disabled topology used here. This example routes everything to
> the primary (ProxySQL still adds connection multiplexing, query rules, and
> stats). To scale reads across the GR secondaries you have two options:
> enable HAProxy (`spec.proxy.haproxy.enabled`) and add a reader hostgroup
> pointing at `cluster1-haproxy:3307`, or expose the pods individually
> (`spec.mysql.expose.enabled`) and split writer/reader by `read_only` with a
> `mysqlReplicationHostgroups` entry — see the
> [mariadb-operator example](../mariadb-operator/) for that pattern.

## What this example creates

- A `PerconaServerMySQL` (3 nodes), HAProxy/Router disabled, `exposePrimary` on.
- A `ProxySQLCluster` (3 replicas).
- A `ProxySQLConfig` with hostgroup 0 → `cluster1-mysql-primary`.

## Install order

```bash
# 1. PS Operator (once per cluster).
helm repo add percona https://percona.github.io/percona-helm-charts/
helm install ps-operator percona/ps-operator -n ps-operator --create-namespace

# 2. Namespace + backend secret + PerconaServerMySQL CR.
kubectl create namespace percona-ps-demo
kubectl apply -f backend.yaml

# 3. Wait for it to become Ready (3-5 min).
kubectl -n percona-ps-demo wait perconaservermysql/cluster1 --for=jsonpath='{.status.state}'=ready --timeout=10m

# 4. ProxySQL operator (once per cluster) — see examples/README.md.

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml
```

## Smoke test

```bash
ROOT_PW=$(kubectl -n percona-ps-demo get secret cluster1-secrets -o jsonpath='{.data.root}' | base64 -d)
kubectl -n percona-ps-demo run -it --rm mysql-cli --image=mysql:8.0 --restart=Never -- \
  mysql -h proxysql -P 6033 -uroot -p"$ROOT_PW" -e "SHOW DATABASES"
```

Or run the [sysbench loadgen](../../loadgen/sysbench.yaml).
