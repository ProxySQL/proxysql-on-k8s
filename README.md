# proxysql-on-k8s

[![ci](https://github.com/ProxySQL/proxysql-on-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/ProxySQL/proxysql-on-k8s/actions/workflows/ci.yaml)

Kubernetes operator and Helm charts for [ProxySQL 3.x](https://proxysql.com/)
(MySQL `6033` + PostgreSQL `6133`).

> Backend cookbook examples + full architecture docs land in the next
> follow-up PR.
> For the legacy v1 demo charts, see [`ProxySQL/kubernetes`](https://github.com/ProxySQL/kubernetes).

## What's in the box

### Helm charts

| Chart | Workload | Purpose |
| --- | --- | --- |
| [`charts/proxysql-operator/`](./charts/proxysql-operator) | Operator `Deployment` | Operator install — CRDs + controller manager + RBAC |
| [`charts/proxysql/`](./charts/proxysql) | `Deployment` | Standalone data plane. Backend-agnostic. Operator-less. |
| [`charts/proxysql-cluster/`](./charts/proxysql-cluster) | `StatefulSet` + PVC | Standalone control-plane node, persistent. Operator-less. |

Most users want the operator chart, which manages both data-plane and
control-plane pods via CRDs. The other two are for environments where
running an operator isn't desirable.

### CRDs (`proxysql.com/v1alpha1`)

| Kind | Short | What it owns |
| --- | --- | --- |
| `ProxySQLCluster` (`pxc`) | A set of ProxySQL pods | `StatefulSet`, headless + ClusterIP Services, `Secret` (admin/radmin/monitor passwords minted by the operator), `ConfigMap`, `PodDisruptionBudget`, optional `ServiceMonitor` |
| `ProxySQLConfig` (`pxcfg`) | Declarative ProxySQL configuration pushed to a `ProxySQLCluster` | Nothing — runs SQL writes against each replica's admin port |

## Quick start

```bash
# 1. Install the operator.
helm install proxysql-operator ./charts/proxysql-operator \
  -n proxysql-system --create-namespace

# 2. Create a ProxySQLCluster + ProxySQLConfig.
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

# 3. Read the minted admin password and connect.
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
make e2e               # full kind e2e — see test/e2e/proxysql-cluster.sh

cd operator
make test              # go test ./... (unit + envtest)
make lint              # golangci-lint
make run               # run the manager locally against the current kubectx
```

CI runs all of the above on every PR — see [`.github/workflows/ci.yaml`](./.github/workflows/ci.yaml).

## License

Apache-2.0 — see [`LICENSE`](./LICENSE).
