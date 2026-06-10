# Crunchy PostgreSQL Operator (PGO)

ProxySQL **3.x** (PostgreSQL protocol enabled) in front of a
[Crunchy PGO](https://access.crunchydata.com/documentation/postgres-operator/v5/)
`PostgresCluster`. Crunchy uses Patroni for HA and exposes:

| Service            | Role                                |
| ------------------ | ----------------------------------- |
| `<name>-primary`   | always the current Patroni leader   |
| `<name>-replicas`  | hot standbys only                   |
| `<name>-pgbouncer` | optional connection pooler (unused) |

ProxySQL replaces pgbouncer and adds query routing on top.

> ⚠️ **TLS caveat.** PGO generates a `pg_hba.conf` made of `hostssl` rules
> only — by default it **refuses non-TLS connections**. The ProxySQL
> operator's `pgsqlServers` entries can't enable backend TLS yet (the pgsql
> side of the CRD has no `useSSL` field), so `backend.yaml` appends a
> `host all all all scram-sha-256` rule via
> `spec.patroni.dynamicConfiguration.postgresql.pg_hba`. Passwords are still
> SCRAM-verified, but the ProxySQL→Postgres hop is unencrypted — fine for a
> demo, a real deployment should keep that traffic inside a service mesh or
> wait for backend-TLS support in the CRD. PGO's mandatory
> certificate-authenticated replication entries are unaffected.

## What this example creates

- A 1-instance `PostgresCluster` (kind-sized — set `replicas: 3` for real HA).
- A `ProxySQLCluster` with the PostgreSQL listener enabled (`6133`).
- A `ProxySQLConfig` referencing PGO's per-user secrets directly
  (`hippo-pguser-app`, `hippo-pguser-postgres`).

## Install order

```bash
# 1. Crunchy PGO (once per cluster), pinned — Crunchy publishes the chart on
#    their OCI registry.
helm install pgo oci://registry.developers.crunchydata.com/crunchydata/pgo \
  --version 5.8.3 -n postgres-operator --create-namespace

# 2. Backend.
kubectl apply -f backend.yaml

# 3. Wait for the Patroni leader to be up.
kubectl -n crunchy-demo wait pod \
  -l postgres-operator.crunchydata.com/cluster=hippo,postgres-operator.crunchydata.com/role=master \
  --for=condition=Ready --timeout=10m

# 4. ProxySQL operator — see examples/README.md.

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml
```

## Smoke test

```bash
# PGO creates `hippo-pguser-app` for the `app` user we declared in backend.yaml.
PG_PW=$(kubectl -n crunchy-demo get secret hippo-pguser-app -o jsonpath='{.data.password}' | base64 -d)

kubectl -n crunchy-demo run -it --rm pg-cli --image=postgres:16 --restart=Never --env=PGPASSWORD="$PG_PW" -- \
  psql -h proxysql -p 6133 -U app -d app -c "SELECT current_database(), pg_is_in_recovery()"
```

You should get `app` and `f` — the query went through ProxySQL's `6133`
listener to the Patroni leader. Or run the
[pgbench loadgen](../../loadgen/pgbench.yaml) for sustained traffic.
