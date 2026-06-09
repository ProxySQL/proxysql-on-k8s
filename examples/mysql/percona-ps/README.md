# Percona Operator for MySQL Server

ProxySQL in front of the [Percona Operator for MySQL Server](https://docs.percona.com/percona-operator-for-mysql/ps/)
(PS Operator). This one uses MySQL Group Replication under the hood, but
unlike Oracle's operator it exposes ready-made `*-mysql-primary` and
`*-mysql-replicas` Services so we can wire ProxySQL straight to them.

## What this example creates

- A `PerconaServerMySQL` (3 nodes) with HAProxy disabled (we use ProxySQL
  instead).
- A `ProxySQLCluster` (3 replicas).
- A `ProxySQLConfig` with hostgroup 0 → `cluster1-mysql-primary` and
  hostgroup 1 → `cluster1-mysql-replicas`.

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
