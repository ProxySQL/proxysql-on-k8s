# Helm charts

Three charts make up the ProxySQL-on-Kubernetes deployment:

| Chart | Purpose | Workload |
|---|---|---|
| [`proxysql/`](./proxysql) | Standalone, backend-agnostic data plane. Serves MySQL (`6033`) and PostgreSQL (`6133`) from a single pod. Configuration is rendered from Helm values into a ConfigMap-mounted `proxysql.cnf`. Operator-less. | `Deployment` |
| [`proxysql-cluster/`](./proxysql-cluster) | Standalone control-plane node of a ProxySQL Cluster. Persists `/var/lib/proxysql` on a PVC. Same values-into-ConfigMap configuration model as `proxysql/`. Operator-less. | `StatefulSet` |
| [`proxysql-operator/`](./proxysql-operator) | Installs the operator (CRDs + manager Deployment + RBAC). Reconciles `ProxySQLCluster` and `ProxySQLConfig` CRs in the `proxysql.com/v1alpha1` API group. With the operator, configuration is pushed to ProxySQL's admin port at runtime rather than baked into a ConfigMap. | Operator (`Deployment`) |

The two standalone charts are for environments where running an operator
isn't desirable. Most users want `proxysql-operator`, which manages both the
data-plane and control-plane pods via CRDs. See each chart's `values.yaml` for
the full configuration surface.
