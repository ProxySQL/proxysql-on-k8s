# Crunchy PostgreSQL Operator (PGO)

ProxySQL **3.x** in front of a [Crunchy PGO](https://access.crunchydata.com/documentation/postgres-operator/latest/)
`PostgresCluster`. Crunchy uses Patroni for HA and exposes:

| Service               | Role                                |
| --------------------- | ----------------------------------- |
| `<name>-primary`      | always the current Patroni leader   |
| `<name>-replicas`     | hot standbys only                   |
| `<name>-pgbouncer`    | optional connection pooler (unused) |

ProxySQL replaces pgbouncer and adds query routing on top.

## What this example creates

- A 3-instance `PostgresCluster` (Crunchy PGO).
- A `ProxySQLCluster` with the PostgreSQL listener enabled.
- A `ProxySQLConfig` referencing PGO's per-user secrets directly
  (`<name>-pguser-<user>` for both the in-spec users and the system
  `postgres` superuser).

## Install order

```bash
# 1. Crunchy PGO. The Helm chart isn't published to a public repo; the
#    upstream-supported path is the Postgres Operator Examples repo.
git clone --depth 1 https://github.com/CrunchyData/postgres-operator-examples
kubectl apply -k postgres-operator-examples/kustomize/install/namespace
kubectl apply --server-side -k postgres-operator-examples/kustomize/install/default

# 2. Backend.
kubectl create namespace crunchy-demo
kubectl apply -f backend.yaml

# 3. Wait for the cluster to reach the `pghoard.crunchydata.com/cluster=hippo,role=master` state.
kubectl -n crunchy-demo wait postgrescluster/hippo --for=condition=Ready --timeout=10m

# 4. ProxySQL operator — see examples/README.md.

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml
```

## Smoke test

```bash
# PGO creates `hippo-pguser-app` for the `app` user we defined in backend.yaml.
PG_PW=$(kubectl -n crunchy-demo get secret hippo-pguser-app -o jsonpath='{.data.password}' | base64 -d)

kubectl -n crunchy-demo run -it --rm pg-cli --image=postgres:16 --restart=Never --env=PGPASSWORD="$PG_PW" -- \
  psql -h proxysql -p 6133 -U app -d app -c "SELECT pg_is_in_recovery()"
```
