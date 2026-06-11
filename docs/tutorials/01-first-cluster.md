# Tutorial 1 — Your first ProxySQL cluster

**What you'll learn**

- How to deploy a `ProxySQLCluster` and a `ProxySQLConfig`, one at a time
- What Kubernetes objects the operator creates on your behalf
- How to read the status: phase, conditions, endpoints, sync columns
- What the admin port is and how to query it as `radmin`

**Prerequisites**

- A Kubernetes cluster and `kubectl` (a local kind cluster is fine)
- `helm`
- No prior steps — this is the first tutorial. If you raced through the
  [quickstart](../quickstart.md), this covers the same ground slowly and
  explains what actually happened.

## 1. Install the operator

```sh
helm repo add proxysql https://proxysql.github.io/proxysql-on-k8s
helm repo update
helm install proxysql-operator proxysql/proxysql-operator \
  --namespace proxysql-system --create-namespace
kubectl -n proxysql-system rollout status deploy/proxysql-operator --timeout=120s
```

```
deployment "proxysql-operator" successfully rolled out
```

> [!TIP]
> Working from a repo checkout against a kind cluster? Build and install
> locally instead (this is what was used to validate this tutorial):
>
> ```sh
> make operator-image-kind IMG=proxysql-operator:docs KIND_CLUSTER=<your-kind-cluster>
> helm install proxysql-operator charts/proxysql-operator \
>   --set image.repository=proxysql-operator \
>   --set image.tag=docs \
>   --set image.pullPolicy=Never \
>   -n proxysql-system --create-namespace
> ```

The chart installs two CRDs (`proxysqlclusters.proxysql.com`,
`proxysqlconfigs.proxysql.com`), the manager Deployment, and its RBAC. See
[user-guide/installation.md](../user-guide/installation.md) for all install
options.

## 2. Deploy a backend to proxy

ProxySQL is a proxy; it needs a database behind it. For this tutorial that's
a single throwaway MySQL pod — wiring up real, replicated backends is covered
in [user-guide/backends.md](../user-guide/backends.md).

```sh
kubectl create namespace proxysql-tutorial
kubectl -n proxysql-tutorial apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: app-user
stringData:
  password: app-secret-pw
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mysql
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
spec:
  selector: {app: mysql}
  ports: [{port: 3306}]
EOF
kubectl -n proxysql-tutorial rollout status deploy/mysql --timeout=180s
```

Note the `app-user` Secret: the application user's password lives in a
Kubernetes Secret, never in a CR. You'll reference it from `ProxySQLConfig`
in step 5.

## 3. Create the ProxySQLCluster

The `ProxySQLCluster` CR describes the *pods* — how many, which image, which
protocol ports, persistence. It says nothing about backends or users; that's
deliberate ([why two CRDs?](../architecture.md#why-a-separate-proxysqlconfig-crd)).

```sh
kubectl -n proxysql-tutorial apply -f - <<'EOF'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: proxysql
spec:
  replicas: 1
EOF
kubectl -n proxysql-tutorial wait --for=condition=Ready pod/proxysql-0 --timeout=180s
kubectl -n proxysql-tutorial get pxc proxysql
```

```
NAME       REPLICAS   READY   PHASE     AGE
proxysql   1          1       Running   10s
```

Everything else defaulted: image `proxysql/proxysql:3.0`, MySQL protocol on
6033, admin on 6032, metrics on 6070, persistence on (a 1Gi PVC per pod).
The full field list is in
[reference/proxysqlcluster.md](../reference/proxysqlcluster.md).

## 4. What did the operator create?

```sh
kubectl -n proxysql-tutorial get statefulset,pods,services,secrets,pvc,pdb
```

```
NAME                        READY   AGE
statefulset.apps/proxysql   1/1     14s

NAME                         READY   STATUS    RESTARTS   AGE
pod/mysql-746d789b67-jh6cl   1/1     Running   0          29s
pod/proxysql-0               1/1     Running   0          14s

NAME                        TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)                      AGE
service/mysql               ClusterIP   10.96.134.43   <none>        3306/TCP                     29s
service/proxysql            ClusterIP   10.96.12.242   <none>        6033/TCP,6032/TCP,6070/TCP   14s
service/proxysql-headless   ClusterIP   None           <none>        6033/TCP,6032/TCP            14s

NAME                  TYPE     DATA   AGE
secret/app-user       Opaque   1      29s
secret/proxysql       Opaque   3      14s
secret/proxysql-cnf   Opaque   1      14s

NAME                                    STATUS   VOLUME       CAPACITY   ACCESS MODES   STORAGECLASS   AGE
persistentvolumeclaim/data-proxysql-0   Bound    pvc-1171...  1Gi        RWO            standard       14s
```

Owned by the `ProxySQLCluster` (deleting the CR deletes them all):

| Object | Purpose |
| --- | --- |
| `statefulset/proxysql` | Runs the ProxySQL pods (`proxysql-0`, `proxysql-1`, …) with stable names. |
| `service/proxysql` | The client-facing Service — point applications here (6033 MySQL, 6032 admin, 6070 metrics). |
| `service/proxysql-headless` | Gives each pod a stable DNS name (`proxysql-0.proxysql-headless...`); used for ProxySQL's own cluster sync, not for clients. |
| `secret/proxysql` | Operator-minted random passwords: `admin-password`, `radmin-password`, `monitor-password`. Bring your own via `spec.auth.secretName` ([security guide](../user-guide/security.md)). |
| `secret/proxysql-cnf` | The bootstrap `proxysql.cnf` — just enough config to start the admin port. It's a Secret because it embeds those passwords. |
| `persistentvolumeclaim/data-proxysql-0` | `/var/lib/proxysql`, where ProxySQL keeps its own SQLite db. Disable with `spec.persistence.enabled: false`. |

No PodDisruptionBudget yet — the operator only creates one when
`replicas > 1` (you'll see it appear in
[tutorial 4](04-high-availability.md)). And note what is *not* there: no
ConfigMap of SQL, no sidecar that templates config. Backends and users reach
ProxySQL another way (step 5).

Check the admin Secret's keys:

```sh
kubectl -n proxysql-tutorial describe secret proxysql | tail -5
```

```
Data
====
admin-password:    32 bytes
monitor-password:  32 bytes
radmin-password:   32 bytes
```

## 5. Reading status: phase, conditions, endpoints

```sh
kubectl -n proxysql-tutorial get pxc proxysql -o yaml | sed -n '/^status:/,$p'
```

```yaml
status:
  adminSecretName: proxysql
  conditions:
  - lastTransitionTime: "2026-06-11T09:47:41Z"
    message: 1/1 replicas ready
    reason: AllReplicasReady
    status: "True"
    type: Available
  - lastTransitionTime: "2026-06-11T09:47:41Z"
    message: no rollout in progress
    reason: Steady
    status: "False"
    type: Progressing
  endpoints:
    admin: proxysql.proxysql-tutorial.svc:6032
    metrics: proxysql.proxysql-tutorial.svc:6070
    mysql: proxysql.proxysql-tutorial.svc:6033
  observedGeneration: 1
  phase: Running
  readyReplicas: 1
  replicas: 1
  updatedReplicas: 1
```

- **`phase`** is a one-word summary for dashboards: `Pending` → `Creating` →
  `Running`, with `Updating` during rollouts and `Degraded` on errors.
- **`conditions`** are the source of truth: `Available` (all replicas
  ready), `Progressing` (rollout in flight), `Degraded` (something broke).
- **`endpoints`** are ready-made `host:port` strings per enabled surface, so
  nothing downstream has to re-derive Service names and ports.

Full details in [reference/status.md](../reference/status.md).

## 6. Configure it: the ProxySQLConfig

The `ProxySQLConfig` CR is the *configuration*: backend servers, application
users, query rules, variables. The operator connects to each ProxySQL pod's
admin port and applies it with SQL (`INSERT` + `LOAD ... TO RUNTIME` +
`SAVE ... TO DISK`) — and keeps re-asserting it.

```sh
kubectl -n proxysql-tutorial apply -f - <<'EOF'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: proxysql
spec:
  clusterRef: {name: proxysql}
  mysqlServers:
    - hostgroup: 0
      hostname: mysql.proxysql-tutorial.svc.cluster.local
      port: 3306
  mysqlUsers:
    - username: app
      defaultHostgroup: 0
      defaultSchema: appdb
      passwordSecretRef: {name: app-user, key: password}
  mysqlVariables:
    mysql-monitor_enabled: "false"
EOF
kubectl -n proxysql-tutorial get pxcfg
```

```
NAME       CLUSTER    SYNCED   DRIFTED   LAST-SYNC   AGE
proxysql   proxysql   1                  10s         10s
```

`SYNCED 1` = the config reached 1 of 1 ready replicas. (`DRIFTED` stays
empty/0 unless a runtime check finds a replica diverging — more on that in
[tutorial 4](04-high-availability.md).)

Two things worth noticing:

- `passwordSecretRef` — the operator resolves the password from the Secret
  at sync time; it never appears in the CR or its status.
- `mysql-monitor_enabled: "false"` — our demo backend has no `monitor` user,
  so we switch ProxySQL's health-check module off rather than let it shun a
  perfectly reachable server. With a real backend you'd create the monitor
  user instead ([backends guide](../user-guide/backends.md)).

## 7. The admin port (6032)

Every ProxySQL pod has an admin interface on port 6032. It speaks the
**MySQL wire protocol** (even for PostgreSQL-related tables), and the account
for remote access is **`radmin`** — ProxySQL restricts the `admin` user to
localhost. The operator uses exactly this port and account to push your
config; you can use it too, for inspection:

```sh
RADMIN_PW="$(kubectl -n proxysql-tutorial get secret proxysql -o jsonpath='{.data.radmin-password}' | base64 -d)"
kubectl -n proxysql-tutorial run admin-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD="$RADMIN_PW" -- \
  mysql -h proxysql -P6032 -uradmin -e "SELECT hostgroup_id, hostname, port, status FROM runtime_mysql_servers; SELECT username, default_hostgroup, default_schema, frontend, backend FROM runtime_mysql_users"
```

```
hostgroup_id	hostname	port	status
0	mysql.proxysql-tutorial.svc.cluster.local	3306	ONLINE
username	default_hostgroup	default_schema	frontend	backend
app	0	appdb	1	0
app	0	appdb	0	1
```

The `runtime_*` tables show what ProxySQL is *actually running* — exactly
what the operator wrote. (Users appear twice by design: ProxySQL keeps one
row for the client-facing credential, `frontend=1`, and one for the
credential it uses towards the backend, `backend=1`.) The admin-table cheat
sheet is in [reference/admin-tables.md](../reference/admin-tables.md).

## 8. Run queries through the data plane (6033)

```sh
kubectl -n proxysql-tutorial run mysql-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD=app-secret-pw -- \
  mysql -h proxysql -P6033 -uapp -e "CREATE TABLE IF NOT EXISTS greetings (id INT PRIMARY KEY AUTO_INCREMENT, msg VARCHAR(64)); INSERT INTO greetings (msg) VALUES ('hello from ProxySQL'); SELECT * FROM greetings"
```

```
id	msg
1	hello from ProxySQL
```

Your application talks to `proxysql:6033` with its own credentials
(`app` / the Secret's password); ProxySQL holds the pooled connections to
the real database.

## Clean up

**Continuing to [tutorial 2](02-query-routing.md)? Skip this** — it builds
on this namespace.

```sh
kubectl delete namespace proxysql-tutorial
```

## Next

[Tutorial 2 — Query routing: users and rules](02-query-routing.md): add a
second backend and route, rewrite, and cache queries with `mysqlQueryRules`.
