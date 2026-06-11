# Quickstart: ProxySQL on Kubernetes in 5 minutes

Four steps: install the operator, apply one manifest, run a query through
ProxySQL, tear it down. On a warm cluster (images pulled) the whole thing is
well under five minutes — the validated run for this page took 26 seconds of
wall-clock after the operator install.

You need a Kubernetes cluster (a local [kind](https://kind.sigs.k8s.io/)
cluster is fine: `kind create cluster`), `kubectl`, and `helm`.

## Step 1 — Install the operator

```sh
helm repo add proxysql https://proxysql.github.io/proxysql-on-k8s
helm repo update
helm install proxysql-operator proxysql/proxysql-operator \
  --namespace proxysql-system --create-namespace
```

> [!TIP]
> Working from a checkout of this repo instead (no published image needed)?
> Build the image straight into your kind cluster and install the local chart —
> this is the exact path used to validate this page:
>
> ```sh
> make operator-image-kind IMG=proxysql-operator:docs KIND_CLUSTER=<your-kind-cluster>
> helm install proxysql-operator charts/proxysql-operator \
>   --set image.repository=proxysql-operator \
>   --set image.tag=docs \
>   --set image.pullPolicy=Never \
>   -n proxysql-system --create-namespace
> ```

Wait for the manager to come up:

```sh
kubectl -n proxysql-system rollout status deploy/proxysql-operator --timeout=120s
```

## Step 2 — Apply one manifest

This single manifest creates a namespace, a throwaway MySQL backend, a
one-pod ProxySQL cluster, and the ProxySQL configuration (backend + user)
that the operator pushes into it:

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: quickstart
---
# A throwaway single-node MySQL backend (replace with your real database).
apiVersion: v1
kind: Secret
metadata:
  name: app-user
  namespace: quickstart
stringData:
  password: app-secret-pw
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mysql
  namespace: quickstart
spec:
  replicas: 1
  selector:
    matchLabels: {app: mysql}
  template:
    metadata:
      labels: {app: mysql}
    spec:
      containers:
        - name: mysql
          image: mysql:8.0
          env:
            - {name: MYSQL_ROOT_PASSWORD, value: root-secret-pw}
            - {name: MYSQL_DATABASE, value: appdb}
            - {name: MYSQL_USER, value: app}
            - {name: MYSQL_PASSWORD, value: app-secret-pw}
          ports: [{containerPort: 3306}]
          readinessProbe:
            exec:
              command: ["mysqladmin", "ping", "-h", "127.0.0.1", "-uroot", "-proot-secret-pw"]
            initialDelaySeconds: 8
            periodSeconds: 4
---
apiVersion: v1
kind: Service
metadata:
  name: mysql
  namespace: quickstart
spec:
  selector: {app: mysql}
  ports: [{port: 3306}]
---
# The ProxySQL pods.
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: proxysql
  namespace: quickstart
spec:
  replicas: 1
  persistence:
    enabled: false   # demo only — keep the default (true) in production
---
# The ProxySQL configuration: backends, users, variables.
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: proxysql
  namespace: quickstart
spec:
  clusterRef: {name: proxysql}
  mysqlServers:
    - hostgroup: 0
      hostname: mysql.quickstart.svc.cluster.local
      port: 3306
  mysqlUsers:
    - username: app
      defaultHostgroup: 0
      defaultSchema: appdb
      passwordSecretRef: {name: app-user, key: password}
  mysqlVariables:
    # The demo backend has no "monitor" user; disable health checks so
    # ProxySQL doesn't shun a perfectly reachable server.
    mysql-monitor_enabled: "false"
EOF
```

Wait for both to come up:

```sh
kubectl -n quickstart wait --for=condition=Ready pod/proxysql-0 --timeout=180s
kubectl -n quickstart rollout status deploy/mysql --timeout=180s
```

## Step 3 — Verify and run a query

`pxc` is the short name for `proxysqlcluster`, `pxcfg` for `proxysqlconfig`:

```sh
kubectl -n quickstart get pxc,pxcfg
```

```
NAME                                    REPLICAS   READY   PHASE     AGE
proxysqlcluster.proxysql.com/proxysql   1          1       Running   14s

NAME                                   CLUSTER    SYNCED   DRIFTED   LAST-SYNC   AGE
proxysqlconfig.proxysql.com/proxysql   proxysql   1                  7s          14s
```

`PHASE: Running` means every replica is ready; `SYNCED: 1` means the operator
pushed your config to 1 of 1 replicas. The status also publishes ready-made
endpoints:

```sh
kubectl -n quickstart get pxc proxysql -o jsonpath='{.status.endpoints}'
```

```
{"admin":"proxysql.quickstart.svc:6032","metrics":"proxysql.quickstart.svc:6070","mysql":"proxysql.quickstart.svc:6033"}
```

Now run a real query through ProxySQL's MySQL port (6033):

```sh
kubectl -n quickstart run mysql-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD=app-secret-pw -- \
  mysql -h proxysql -P6033 -uapp -e "SELECT VERSION(), DATABASE()"
```

```
VERSION()	DATABASE()
8.0.46	appdb
```

That query went client → ProxySQL → MySQL and back. ProxySQL authenticated
the `app` user itself (the operator pushed the credential from the `app-user`
Secret) and routed the query to hostgroup 0.

## Step 4 — Tear down

```sh
kubectl delete namespace quickstart
```

Optionally remove the operator too:

```sh
helm uninstall proxysql-operator -n proxysql-system
```

## Where to next

- [Tutorial 1: your first cluster](tutorials/01-first-cluster.md) — the same
  ground at walking pace: what the operator created and how to read status.
- [Tutorial 2: query routing](tutorials/02-query-routing.md) — users, query
  rules, rewrites, and the query cache.
- [User guide: installation](user-guide/installation.md) — production
  install options for the operator chart.
- [User guide: backends](user-guide/backends.md) — connecting real replicated
  backends (MySQL operators, CloudNativePG, …) instead of a demo pod.
- [Reference: ProxySQLCluster](reference/proxysqlcluster.md) and
  [ProxySQLConfig](reference/proxysqlconfig.md) — every field.
