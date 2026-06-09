# proxysql-on-k8s

[![ci](https://github.com/ProxySQL/proxysql-on-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/ProxySQL/proxysql-on-k8s/actions/workflows/ci.yaml)

Helm charts for running [ProxySQL 3.x](https://proxysql.com/) on Kubernetes
(MySQL `6033` + PostgreSQL `6133`).

> The operator, CRDs, examples, and full documentation land in follow-up
> pull requests. For the legacy v1 demo charts, see
> [`ProxySQL/kubernetes`](https://github.com/ProxySQL/kubernetes); a migration
> guide ships in a later PR.

## Charts

| Chart | Workload | Purpose |
| --- | --- | --- |
| [`charts/proxysql/`](./charts/proxysql) | `Deployment` | Backend-agnostic data plane. Stateless, horizontally scalable. |
| [`charts/proxysql-cluster/`](./charts/proxysql-cluster) | `StatefulSet` + PVC | Persistent control-plane node, peer of a ProxySQL Cluster. |

Both charts run upstream `proxysql/proxysql:3.0` (no forks). Both default to
the Pod Security Standards `restricted` profile: non-root, read-only root
filesystem, all capabilities dropped, `RuntimeDefault` seccomp.

## Quick start

```bash
helm install proxysql ./charts/proxysql \
  -n proxysql --create-namespace \
  --set "backends.mysql[0].hostgroup=0,backends.mysql[0].host=mysql.default.svc,backends.mysql[0].port=3306"
```

Backends live under `backends.mysql` / `backends.pgsql` (each a list of
`{ hostgroup, host, port }`). See each chart's `values.yaml` for the full
configuration surface.

## Development

```bash
make lint              # helm lint every chart
make template          # helm template every chart (sanity render)
make kubeconform       # render + kubeconform schema validation
make kind-up           # create a local kind cluster
make kind-down         # tear it down
```

## License

Apache-2.0 — see [`LICENSE`](./LICENSE).
