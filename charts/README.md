# Helm charts

Three charts make up the v2 ProxySQL-on-Kubernetes deployment:

| Chart | Purpose | Workload |
|---|---|---|
| [`proxysql/`](./proxysql) | Backend-agnostic data plane. Supports MySQL (`6033`) and PostgreSQL (`6133`) protocols from a single pod. Deployed as `topology: layer` or `topology: sidecar`. | `Deployment` |
| [`proxysql-cluster/`](./proxysql-cluster) | Control-plane node for the ProxySQL Cluster. Persists `/var/lib/proxysql`. Managed by the operator — config does not live in a ConfigMap. | `StatefulSet` |
| [`proxysql-operator/`](./proxysql-operator) | Installs the operator (CRDs + manager Deployment + RBAC). Reconciles `ProxySQLCluster` and `ProxySQLConfig` CRs in the `proxysql.com/v1alpha1` API group. | Operator |

All three are scaffolded in Phase 0 and built out in Phases 1 and 3 of [`docs/v2-plan.md`](../docs/v2-plan.md).
