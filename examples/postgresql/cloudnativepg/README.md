# CloudNativePG

ProxySQL **3.x** (PostgreSQL protocol enabled) in front of a
[CloudNativePG](https://cloudnative-pg.io) `Cluster`.

CNPG exposes three Services per Cluster:

| Service     | Role                        | Used as     |
| ----------- | --------------------------- | ----------- |
| `<name>-rw` | primary (read-write)        | hostgroup 0 |
| `<name>-ro` | hot standbys (read-only)    | hostgroup 1 |
| `<name>-r`  | primary + standbys combined | unused here |

We point ProxySQL at the first two. CNPG handles failover; the `-rw`
Service follows the new primary automatically, so ProxySQL doesn't need
its own monitor for promotion.

## What this example creates

- A 3-node `Cluster` (CNPG) with persistent storage.
- A `ProxySQLCluster` with the PostgreSQL listener enabled (`6133`).
- A `ProxySQLConfig` declaring the two hostgroups + a `pg` user reading
  its password from the CNPG-managed secret.

## Install order

```bash
# 1. CloudNativePG operator.
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace

# 2. Backend.
kubectl create namespace cnpg-demo
kubectl apply -f backend.yaml

# 3. Wait for the Cluster to be healthy.
kubectl -n cnpg-demo wait cluster/pg --for=condition=Ready --timeout=10m

# 4. ProxySQL operator — see examples/README.md.

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml
```

## Smoke test

```bash
# CNPG provisions an app secret named "<cluster>-app" by default.
PG_PW=$(kubectl -n cnpg-demo get secret pg-app -o jsonpath='{.data.password}' | base64 -d)

kubectl -n cnpg-demo run -it --rm pg-cli --image=postgres:16 --restart=Never --env=PGPASSWORD="$PG_PW" -- \
  psql -h proxysql -p 6133 -U app -d app -c "SELECT pg_is_in_recovery(), current_setting('cluster_name')"
```

Run twice — you should see `pg_is_in_recovery=false` (primary) when the
query routes to hostgroup 0 and `true` (hot standby) when it routes to
hostgroup 1.
