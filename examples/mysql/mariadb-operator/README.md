# mariadb-operator

ProxySQL in front of the [mariadb-operator](https://github.com/mariadb-operator/mariadb-operator)
running async primary/replica MariaDB. This is the cleanest read/write
split: `read_only=ON` on replicas, the operator promotes one on failure,
and ProxySQL's standard `mysqlReplicationHostgroups` table follows the
flag automatically.

## What this example creates

- A `MariaDB` CR (1 primary + 2 replicas, async replication).
- A `ProxySQLCluster` (3 replicas).
- A `ProxySQLConfig` with:
  - hostgroup 0 (writer) and hostgroup 1 (reader),
  - a `mysqlReplicationHostgroups` row that tells ProxySQL to follow the
    `read_only` flag — when the operator fails over and clears
    `read_only` on a new primary, ProxySQL moves the row between
    hostgroups within seconds.

> ⚠️ **Monitor password — keep two places in sync.** ProxySQL's monitor
> credentials live in admin variables, which the `ProxySQLConfig` exposes as
> plain strings under `mysqlVariables` (no `secretRef` there). So the monitor
> password is entered in **two** spots that must match exactly:
> `mysqlVariables.mysql-monitor_password` in `proxysql.yaml` **and** the
> `mariadb-monitor` Secret consumed by the `User` CR in `backend.yaml`. Both
> ship with the same `REPLACE-ME-monitor-pw` placeholder; if you change one,
> change the other, or the monitor gets "Access denied", can't read each
> backend's `read_only`, and parks every server in the reader hostgroup
> (leaving the writer hostgroup empty, so writes fail).

## Install order

```bash
# 1. mariadb-operator.
helm repo add mariadb-operator https://helm.mariadb.com/mariadb-operator
helm install mariadb-operator mariadb-operator/mariadb-operator -n mariadb-operator --create-namespace --set ha.enabled=true

# 2. Backend.
kubectl create namespace mariadb-demo
kubectl apply -f backend.yaml

# 3. Wait for the MariaDB to be Ready.
kubectl -n mariadb-demo wait mariadb/mariadb --for=condition=Ready --timeout=10m

# 4. ProxySQL operator — see examples/README.md.

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml
```

## Smoke test

```bash
ROOT_PW=$(kubectl -n mariadb-demo get secret mariadb-root -o jsonpath='{.data.password}' | base64 -d)
kubectl -n mariadb-demo run -it --rm mariadb-cli --image=mariadb:11.4 --restart=Never -- \
  mariadb -h proxysql -P 6033 -uroot -p"$ROOT_PW" -e \
  "SELECT @@hostname, @@read_only"
```

Run repeatedly with `SELECT @@hostname` — you should see only the
read-only replicas, not the primary, because the query is routed to
hostgroup 1.
