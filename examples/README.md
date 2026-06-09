# Examples — wiring ProxySQL into real backend operators

Each directory under `examples/` is a self-contained cookbook for putting
ProxySQL in front of a specific Kubernetes-native database operator. The
shape is identical across flavors:

| File | Contents |
| --- | --- |
| `README.md` | Install order, expected ready state, smoke test |
| `backend.yaml` | The backend operator's own CR — Postgres / MySQL / MariaDB cluster |
| `proxysql.yaml` | `ProxySQLCluster` + `ProxySQLConfig` pointing at that backend |

Apply order is always the same:

```bash
# 1. The backend operator (one-time, install per cluster).
#    See the README in each example for the upstream Helm command.

# 2. Backend CR.
kubectl apply -f backend.yaml

# 3. Wait for it to reach Ready (see backend operator's docs/CRD status).

# 4. ProxySQL operator (also one-time).
helm repo add proxysql https://proxysql.github.io/kubernetes
helm install proxysql-operator proxysql/proxysql-operator -n proxysql-system --create-namespace

# 5. ProxySQL cluster + config.
kubectl apply -f proxysql.yaml

# 6. Smoke test (see examples/loadgen/).
```

## Flavors

### MySQL backends

| Example | Backend operator | Service DNS used by ProxySQL |
| --- | --- | --- |
| [`mysql/oracle-mysql-operator/`](mysql/oracle-mysql-operator/) | Oracle's [MySQL Operator for Kubernetes](https://github.com/mysql/mysql-operator) (`mysql.oracle.com/v2.InnoDBCluster`) | `<name>.<ns>.svc` (router) |
| [`mysql/percona-ps/`](mysql/percona-ps/) | [Percona Operator for MySQL Server](https://docs.percona.com/percona-operator-for-mysql/ps/) (`ps.percona.com/v1.PerconaServerMySQL`) | `<name>-mysql-primary`, `<name>-mysql-replicas` |
| [`mysql/percona-pxc/`](mysql/percona-pxc/) | [Percona Operator for PXC](https://docs.percona.com/percona-operator-for-mysql/pxc/) (`pxc.percona.com/v1.PerconaXtraDBCluster`) | `<name>-pxc` |
| [`mysql/mariadb-operator/`](mysql/mariadb-operator/) | [mariadb-operator](https://github.com/mariadb-operator/mariadb-operator) (`k8s.mariadb.com/v1alpha1.MariaDB`) | `<name>-primary`, `<name>-secondary` |

### PostgreSQL backends *(requires the ProxySQL 3.x PostgreSQL protocol — enable `protocols.pgsql.enabled: true` on the cluster)*

| Example | Backend operator | Service DNS used by ProxySQL |
| --- | --- | --- |
| [`postgresql/cloudnativepg/`](postgresql/cloudnativepg/) | [CloudNativePG](https://cloudnative-pg.io) (`postgresql.cnpg.io/v1.Cluster`) | `<name>-rw` (primary), `<name>-ro` (replicas) |
| [`postgresql/crunchy-pgo/`](postgresql/crunchy-pgo/) | [Crunchy PGO](https://access.crunchydata.com/documentation/postgres-operator/latest/) (`postgres-operator.crunchydata.com/v1beta1.PostgresCluster`) | `<name>-primary`, `<name>-replicas` |

### Load generation

| File | Tool | Target protocol |
| --- | --- | --- |
| [`loadgen/sysbench.yaml`](loadgen/sysbench.yaml) | `sysbench` (`oltp_read_write`) | MySQL on port 6033 |
| [`loadgen/pgbench.yaml`](loadgen/pgbench.yaml) | `pgbench` | PostgreSQL on port 6133 |

## Conventions used in every example

- **Namespace:** each example uses its own namespace (`<flavor>-demo`) so they
  don't collide if you install several at once.
- **Backend root/superuser secret:** kept at the backend operator's default
  name. The ProxySQLConfig's `mysqlUsers` / `pgsqlUsers` entries reference
  that same secret via `passwordSecretRef` — no copying credentials around.
- **ProxySQL admin password:** minted by the ProxySQL operator into a
  `Secret` named after the `ProxySQLCluster`. You don't manage it; if you
  need it to run admin queries, read it with
  `kubectl get secret <name> -o jsonpath='{.data.admin-password}' | base64 -d`.
- **Hostgroup numbering:**
  - `0` — writer / primary
  - `1` — reader / replica pool
  - The ProxySQL `mysqlReplicationHostgroups` (MySQL only) keeps the split
    honest by following each backend's `read_only` flag.

If you need a different topology (multi-source, geo-distributed reads,
multi-tenant…) start from the flavor closest to your backend and edit the
`mysqlServers` / `pgsqlServers` lists in `proxysql.yaml`.
