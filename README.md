# ProxySQL on Kubernetes

<p align="center">
<a><img width="100%" src="https://i0.wp.com/proxysql.com/wp-content/uploads/2020/04/ProxySQL-Colour-Logo.png?fit=800%2C278&ssl=1" alt="ProxySQL"></a>
</p>

[![ci](https://github.com/ProxySQL/proxysql-on-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/ProxySQL/proxysql-on-k8s/actions/workflows/ci.yaml)

Run [ProxySQL 3.x](https://proxysql.com/) on Kubernetes the boring way: a
single Helm chart for the operator, one CR per cluster, and a separate CR
for the declarative configuration. Speaks both MySQL (`6033`) and
PostgreSQL (`6133`).

```bash
helm repo add proxysql https://proxysql.github.io/proxysql-on-k8s
helm install proxysql-operator proxysql/proxysql-operator -n proxysql-system --create-namespace
kubectl apply -f https://raw.githubusercontent.com/ProxySQL/proxysql-on-k8s/main/examples/postgresql/cloudnativepg/proxysql.yaml
```

> **Upgrading from v1?** The chart layout, ProxySQL version, and operator
> are all new. See [`docs/migration-from-v1.md`](./docs/migration-from-v1.md)
> for the chart/value rename map and step-by-step migration.

## What's in the box

### Helm charts

| Chart | Purpose | Workload |
| --- | --- | --- |
| [`charts/proxysql-operator/`](./charts/proxysql-operator) | Operator install — CRDs + controller manager + RBAC | `Deployment` (operator) |
| [`charts/proxysql/`](./charts/proxysql) | Standalone data plane. Backend-agnostic. Useful without the operator. | `Deployment` |
| [`charts/proxysql-cluster/`](./charts/proxysql-cluster) | Standalone control-plane node, persistent disk. Useful without the operator. | `StatefulSet` |

The two standalone charts are for environments where running an operator
isn't desirable. **Most users want the operator**, which manages both
data-plane and control-plane pods via CRDs.

### CRDs (`proxysql.com/v1alpha1`)

| Kind | Short | What it owns |
| --- | --- | --- |
| `ProxySQLCluster` (`pxc`) | A set of ProxySQL pods | `StatefulSet`, headless + ClusterIP Services, `Secret` (admin/radmin/monitor passwords, minted by the operator), `ConfigMap` (bootstrap `proxysql.cnf`), `PodDisruptionBudget`, optional `ServiceMonitor` |
| `ProxySQLConfig` (`pxcfg`) | The declarative ProxySQL configuration applied to a `ProxySQLCluster` | Nothing — runs SQL writes against each replica's admin port |

### Backend examples

[`examples/`](./examples) has six end-to-end cookbooks — apply the backend
operator's CR + a ProxySQL CR pair, get a working stack:

- **MySQL family:** [Oracle MySQL Operator](./examples/mysql/oracle-mysql-operator/) (InnoDB Cluster), [Percona PS](./examples/mysql/percona-ps/) (GR), [Percona PXC](./examples/mysql/percona-pxc/) (Galera), [mariadb-operator](./examples/mysql/mariadb-operator/) (async replication)
- **PostgreSQL family:** [CloudNativePG](./examples/postgresql/cloudnativepg/), [Crunchy PGO](./examples/postgresql/crunchy-pgo/)
- **Loadgen:** sysbench (MySQL) + pgbench (PostgreSQL) under [`examples/loadgen/`](./examples/loadgen/)

## Architecture at a glance

```
┌─────────────────────────────────────────────────────────────────────┐
│                    Kubernetes API                                   │
│                                                                     │
│   ProxySQLCluster ─────┐                ProxySQLConfig ─────┐       │
│        │               │                       │            │       │
└────────┼───────────────┼───────────────────────┼────────────┼───────┘
         │ reconciles    │                       │ reconciles │
         ▼               ▼                       ▼            │
    ┌─────────┐  ┌────────────┐            ┌───────────┐      │
    │ Secret  │  │ ConfigMap  │            │ SQL push  │      │
    │ Service │  │ + cnf      │            │ to admin  │──────┘
    │ STS     │  └────────────┘            │ port 6032 │
    │ PDB     │                            └──────┬────┘
    └─────────┘                                   │
         │                                        ▼
         ▼                                ┌──────────────────┐
    ┌───────────────────────────────────────────┐            │
    │            ProxySQL pods                  │            │
    │  ┌───────┐  ┌───────┐  ┌───────┐          │            │
    │  │ mysql │  │ mysql │  │ mysql │ ◄─── traffic from app │
    │  │ 6033  │  │ 6033  │  │ 6033  │                       │
    │  │ pgsql │  │ pgsql │  │ pgsql │                       │
    │  │ 6133  │  │ 6133  │  │ 6133  │                       │
    │  │ admin │  │ admin │  │ admin │ ◄── operator pushes   │
    │  │ 6032  │  │ 6032  │  │ 6032  │     SQL here          │
    │  └───┬───┘  └───┬───┘  └───┬───┘                       │
    └──────┼──────────┼──────────┼───────────────────────────┘
           │          │          │
           ▼          ▼          ▼
        ┌──────────────────────────┐
        │ Backend database         │
        │ (MySQL / PostgreSQL /    │
        │  MariaDB / Percona /     │
        │  Galera / Patroni / …)   │
        └──────────────────────────┘
```

Full design notes in [`docs/architecture.md`](./docs/architecture.md).

## Quick start

```bash
# 1. Install the operator (once per cluster).
helm repo add proxysql https://proxysql.github.io/proxysql-on-k8s
helm install proxysql-operator proxysql/proxysql-operator \
  -n proxysql-system --create-namespace

# 2. Create a ProxySQLCluster + ProxySQLConfig. Either crib from an example
#    under examples/ that matches your backend, or roll your own:
cat <<'EOF' | kubectl apply -f -
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: proxysql
  namespace: default
spec:
  replicas: 3
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: pxcfg
  namespace: default
spec:
  clusterRef:
    name: proxysql
  mysqlServers:
    - { hostgroup: 0, hostname: my-mysql.default.svc, port: 3306 }
  mysqlUsers:
    - username: root
      defaultHostgroup: 0
      passwordSecretRef:
        name: my-mysql-credentials
        key: root-password
EOF

# 3. Connect.
kubectl get secret proxysql -o jsonpath='{.data.admin-password}' | base64 -d
kubectl port-forward svc/proxysql 6033:6033 &
mysql -h 127.0.0.1 -P 6033 -uroot -p
```

## Development

```bash
make lint              # helm lint every chart
make template          # render every chart (sanity)
make kubeconform       # render + kubeconform schema validation
make sync-crds         # regenerate CRDs and copy them into the operator chart
make operator-image    # build the operator container (single-arch, local docker)
make e2e               # full kind e2e suite — see test/e2e/run.sh

cd operator
make test              # go test ./... (unit + envtest)
make lint              # golangci-lint
make run               # run the manager locally against the current kubectx
```

CI runs all of the above on every PR — see [`.github/workflows/ci.yaml`](./.github/workflows/ci.yaml).

## Documentation

- [`docs/architecture.md`](./docs/architecture.md) — operator design, reconcile loops, write strategy
- [`docs/migration-from-v1.md`](./docs/migration-from-v1.md) — mapping from the old chart layout to v2
- [`examples/README.md`](./examples/README.md) — backend cookbook index

## License

Apache-2.0. See [`LICENSE`](./LICENSE).
