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

The `User` CR in `backend.yaml` gets the backend `monitor` account
password from the `mariadb-monitor` Secret. Copy that password to the
ProxySQLCluster auth Secret after the operator creates it. If the
passwords differ, ProxySQL cannot check each backend's `read_only` flag.

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

After the operator creates `secret/proxysql`, copy the backend monitor
password into it. The operator applies the Secret change to running pods:

```bash
MONITOR_B64=$(kubectl -n mariadb-demo get secret mariadb-monitor -o jsonpath='{.data.password}')
kubectl -n mariadb-demo patch secret proxysql --type=merge \
  -p "{\"data\":{\"monitor-password\":\"$MONITOR_B64\"}}"
unset MONITOR_B64
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
