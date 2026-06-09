# Migrating from v1

If you were running the v1 layout (the four sibling charts at the repo
root, the `pondix/proxysqlk8s` image, the bundled `nginx-ingress-patch`,
and a Minikube-first README), the v2 release reshapes essentially
everything. There is no in-place `helm upgrade` path — the chart names,
the value schemas, and the workload definitions are all different.

This document maps the v1 world to the v2 world so you can pick the
shortest migration path.

## Conceptual shifts

| | v1 | v2 |
| --- | --- | --- |
| ProxySQL version | 2.x | 3.x (MySQL + PostgreSQL) |
| Image | Custom fork `pondix/proxysqlk8s:<tag>` (upstream + `mysql-client`) | Upstream `proxysql/proxysql:3.0` |
| Topologies | 4 sibling charts (`proxysql-cluster`, `proxysql-cluster-controller`, `proxysql-cluster-passive`, `proxysql-sidecar`, `proxysql-sidecar-cascade`) | One declarative model: install the operator, create a `ProxySQLCluster` CR + a `ProxySQLConfig` CR |
| Config propagation | `proxysql-cluster-controller`'s [`hg-scheduler.bash`](https://github.com/ProxySQL/kubernetes/blob/master/proxysql-cluster-controller/files/hg-scheduler.bash) polls every 5 s, diffs a script checksum, rewrites `mysql_servers` / `mysql_users` / etc. via the admin port | A Go operator reconciles `ProxySQLConfig` → SQL writes against every replica's admin port. Event-driven; reacts to CRD/pod events directly. |
| Configuration source | Each chart's `files/proxysql.cnf` (embedded `mysql_servers`, `mysql_users`, query rules) | The `ProxySQLConfig` CR. The pod-level cnf is a minimal bootstrap. |
| Credential model | MySQL root password (`XHCO2ydDXj`) hardcoded across four charts and three shell scripts | Operator mints `admin-password`, `radmin-password`, `monitor-password` into a per-cluster Secret. User credentials referenced via `SecretKeyRef` so the backend operator's own credentials are reused without copying. |
| Ingress | Manual nginx-ingress patch (`nginx-ingress-controller-patch.yaml` + `nginx-patch.bash`) to expose port `6033` | Standard `ClusterIP` Service for in-cluster apps; users provide their own ingress/LB if external exposure is needed |
| Sidecar topology | `proxysql-sidecar` + `proxysql-sidecar-cascade` ran ProxySQL next to a sysbench container | Dropped. Sysbench / pgbench moved to standalone Jobs under [`examples/loadgen/`](../examples/loadgen) so loadgen and proxy lifecycle aren't coupled. |
| Repo layout | Charts at root, ProxySQL `.cnf` files, shell scripts | `charts/`, `operator/`, `examples/`, `test/e2e/`, `.github/workflows/` |
| CI | None | `.github/workflows/ci.yaml` runs helm lint, kubeconform, chart-testing, golangci-lint, go test (unit + envtest), Trivy, and a full kind e2e on every PR |

## Chart rename map

| v1 chart | v1 workload | v2 equivalent | Notes |
| --- | --- | --- | --- |
| `proxysql-cluster/` | `Deployment`, replicas=2, embedded `mysql_servers` pointing at `mysql-8` | `ProxySQLCluster` CR + `ProxySQLConfig` CR | The two-chart split disappears: one CR for shape, one for config. |
| `proxysql-cluster-controller/` | `StatefulSet`, polling scheduler | Replaced by the operator | The whole "controller scheduler" pattern is what the operator is. |
| `proxysql-cluster-passive/` | `Deployment` syncing from the controller via `proxysql_servers` | Multi-replica `ProxySQLCluster` | The operator wires `proxysql_servers` automatically when `replicas > 1`. |
| `proxysql-sidecar/` | `Deployment` with sysbench sidecar | `ProxySQLCluster` + [`examples/loadgen/sysbench.yaml`](../examples/loadgen/sysbench.yaml) | Loadgen is now a separate Job. |
| `proxysql-sidecar-cascade/` | Sidecar pointing at the controller chain | Dropped | Cascaded topology was demo-only; not carried forward. |
| `mysql/values.yaml` | Bitnami MySQL overlay | Use any of [`examples/`](../examples/) | The whole "backend hosted by the same repo" concept is gone — v2 examples wire ProxySQL up to real database operators (Oracle, Percona, MariaDB, CNPG, Crunchy). |

## Value-by-value renames

The v2 chart values share little with v1 by design — most users want the
operator, where the values surface is the `ProxySQLCluster` CR spec, not
chart values. For the two standalone charts (`charts/proxysql/` and
`charts/proxysql-cluster/`), the rough mapping is:

| v1 (`*/values.yaml`) | v2 (`charts/proxysql/values.yaml`) | Notes |
| --- | --- | --- |
| `image.repository: pondix/proxysqlk8s` | `image.repository: proxysql/proxysql` | Upstream image; no fork. |
| `image.tag: 2.x.y` | `image.tag: "3.0"` | ProxySQL 3.x. |
| `replicaCount: 2` | `replicaCount: 2` | Same. |
| `service.port: 6033` | `protocols.mysql.port: 6033` and `protocols.pgsql.port: 6133` | Two listeners. |
| (config inline in `files/proxysql.cnf`) | `backends`, `users`, `queryRules`, `mysqlVariables`, `pgsqlVariables` in values | Cnf is rendered from values. |
| `MYSQL_ROOT_PASSWORD: XHCO2ydDXj` (everywhere) | `auth.adminPassword` in values *or* operator-minted Secret | No more hardcoded passwords. |

If you ran the operator-less standalone chart in v1 (the original
`proxysql-cluster/`), [`charts/proxysql/`](../charts/proxysql) is the
direct replacement.

## Step-by-step migration

This walks the most common path: a v1 install with `proxysql-cluster` +
`proxysql-cluster-controller` + `proxysql-cluster-passive`, talking to a
Bitnami MySQL backend named `mysql-8`.

1. **Take stock.** Capture your current `mysql_servers`, `mysql_users`,
   and `mysql_query_rules` from a live ProxySQL pod, so you have an
   authoritative snapshot to convert into a `ProxySQLConfig`:

   ```bash
   ADMIN=$(kubectl get secret … -o jsonpath='{.data.password}' | base64 -d)
   kubectl exec -it proxysql-cluster-0 -- \
     mysql -h 127.0.0.1 -P 6032 -uradmin -p$ADMIN \
     -e "SELECT * FROM mysql_servers; SELECT username,default_hostgroup FROM mysql_users; SELECT * FROM mysql_query_rules"
   ```

2. **Install the v2 operator into a dedicated namespace.** It doesn't
   touch the v1 install:

   ```bash
   helm repo add proxysql https://proxysql.github.io/proxysql-on-k8s
   helm install proxysql-operator proxysql/proxysql-operator \
     -n proxysql-system --create-namespace
   ```

3. **Create the v2 cluster + config side-by-side with v1.** Pick a new
   release name and namespace; the v1 install stays running:

   ```yaml
   apiVersion: proxysql.com/v1alpha1
   kind: ProxySQLCluster
   metadata: { name: proxysql, namespace: default }
   spec: { replicas: 2 }
   ---
   apiVersion: proxysql.com/v1alpha1
   kind: ProxySQLConfig
   metadata: { name: pxcfg, namespace: default }
   spec:
     clusterRef: { name: proxysql }
     mysqlServers:
       - { hostgroup: 0, hostname: mysql-8.default.svc.cluster.local, port: 3306 }
       - { hostgroup: 1, hostname: mysql-8-slave.default.svc.cluster.local, port: 3306 }
     mysqlReplicationHostgroups:
       - { writerHostgroup: 0, readerHostgroup: 1, checkType: read_only }
     mysqlUsers:
       - username: root
         defaultHostgroup: 0
         passwordSecretRef:
           # If you don't have a Secret yet, put the v1 password here:
           #   kubectl create secret generic mysql-root --from-literal=root-password=XHCO2ydDXj
           name: mysql-root
           key: root-password
   ```

4. **Verify.** Connect through the v2 ProxySQL Service and run the same
   queries you ran against v1. `kubectl get proxysqlconfig pxcfg -o
   yaml` should show `status.syncedReplicas` equal to the cluster's
   replica count.

5. **Switch traffic.** Update your application's `MYSQL_HOST` (or
   equivalent) to the v2 Service. Standard rollover.

6. **Drop v1.** Once nothing else points at the old release:

   ```bash
   helm delete proxysql-cluster proxysql-cluster-controller proxysql-cluster-passive
   ```

## Things that intentionally don't have a v2 equivalent

- **The `pondix/proxysqlk8s` image.** v1 used a forked image that
  bundled `mysql-client` so the in-pod scheduler could shell out to
  `mysql`. v2 doesn't need a mysql client in the pod (the operator
  writes from outside); we run the upstream `proxysql/proxysql:3.0`
  directly. If you maintained your own fork for unrelated reasons, you
  can still override `spec.image.repository` on the CR.

- **`hg-scheduler.bash`.** The whole point of the v2 operator is to
  replace this pattern. If you wrote custom logic *inside* that script
  (extra tables, derived hostgroup rules, etc.) and the CRD's
  declarative fields don't cover it yet, that's the issue to file —
  the right place to add it is the CRD spec, not a per-pod script.

- **`nginx-patch.bash`.** v2 doesn't ship an ingress assumption. If
  you need to expose port 6033 outside the cluster, attach an
  Ingress / LoadBalancer Service / port-forward as your platform
  conventions dictate.

- **`proxysql-admin.bash` and `sysbench-test.bash`.** Replaced by
  `kubectl exec` + the loadgen Jobs under
  [`examples/loadgen/`](../examples/loadgen/).

- **`proxysql-sidecar-cascade/`.** The cascaded sidecar topology was a
  demo for the v1 cluster sync chain; with one operator-managed
  control plane it has no purpose.
