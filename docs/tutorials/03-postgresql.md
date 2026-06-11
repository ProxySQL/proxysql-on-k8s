# Tutorial 3 — PostgreSQL through ProxySQL

**What you'll learn**

- Enabling the PostgreSQL protocol listener (port 6133) on a `ProxySQLCluster`
- Declaring `pgsqlServers`, `pgsqlUsers`, and `pgsqlQueryRules`
- Running `psql` end-to-end through ProxySQL
- Why you still use a MySQL client to inspect the `pgsql_*` admin tables

**Prerequisites**

- Operator installed ([tutorial 1, step 1](01-first-cluster.md#1-install-the-operator)).
- This tutorial is otherwise self-contained — it uses its own namespace.

ProxySQL 3.x speaks the PostgreSQL wire protocol too. The operator treats it
as a first-class surface: a protocol toggle on the cluster, and `pgsql*`
sections in the config that map to ProxySQL's `pgsql_servers` /
`pgsql_users` / `pgsql_query_rules` admin tables.

## 1. Deploy a PostgreSQL backend

```sh
kubectl create namespace pg-tutorial
kubectl -n pg-tutorial apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: pg-app-user
stringData:
  password: pg-secret-pw
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
spec:
  replicas: 1
  selector:
    matchLabels: {app: postgres}
  template:
    metadata:
      labels: {app: postgres}
    spec:
      containers:
        - name: postgres
          image: postgres:16
          env:
            - {name: POSTGRES_USER, value: app}
            - {name: POSTGRES_PASSWORD, value: pg-secret-pw}
            - {name: POSTGRES_DB, value: appdb}
            # ProxySQL authenticates to the backend with md5.
            - {name: POSTGRES_HOST_AUTH_METHOD, value: md5}
          ports: [{containerPort: 5432}]
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "app", "-d", "appdb"]
            initialDelaySeconds: 6
            periodSeconds: 4
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
spec:
  selector: {app: postgres}
  ports: [{port: 5432}]
EOF
kubectl -n pg-tutorial rollout status deploy/postgres --timeout=180s
```

For a production-grade backend (CloudNativePG, Crunchy PGO) see the recipes
under [`examples/postgresql/`](../../examples/postgresql/) and
[user-guide/backends.md](../user-guide/backends.md).

## 2. Create a pgsql-enabled cluster and its config

`protocols.pgsql` is **off by default** — enable it explicitly. Here we also
turn the MySQL listener off to make this a pure PostgreSQL proxy (you can
run both at once; they're independent listeners):

```sh
kubectl -n pg-tutorial apply -f - <<'EOF'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: pgproxy
spec:
  replicas: 1
  protocols:
    mysql: {enabled: false}
    pgsql: {enabled: true}
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: pgproxy
spec:
  clusterRef: {name: pgproxy}
  pgsqlServers:
    - hostgroup: 0
      hostname: postgres.pg-tutorial.svc.cluster.local
      port: 5432
  pgsqlUsers:
    - username: app
      defaultHostgroup: 0
      passwordSecretRef: {name: pg-app-user, key: password}
  pgsqlQueryRules:
    - ruleId: 100
      active: true
      matchPattern: "^SELECT"
      destinationHostgroup: 0
      apply: true
EOF
kubectl -n pg-tutorial wait --for=condition=Ready pod/pgproxy-0 --timeout=180s
kubectl -n pg-tutorial get pxc,pxcfg
```

```
NAME                                   REPLICAS   READY   PHASE     AGE
proxysqlcluster.proxysql.com/pgproxy   1          1       Running   10s

NAME                                  CLUSTER   SYNCED   DRIFTED   LAST-SYNC   AGE
proxysqlconfig.proxysql.com/pgproxy   pgproxy   1                  0s          10s
```

The endpoints now advertise a `pgsql` surface on **6133** (and no `mysql`
one):

```sh
kubectl -n pg-tutorial get pxc pgproxy -o jsonpath='{.status.endpoints}'
```

```
{"admin":"pgproxy.pg-tutorial.svc:6032","metrics":"pgproxy.pg-tutorial.svc:6070","pgsql":"pgproxy.pg-tutorial.svc:6133"}
```

> [!NOTE]
> Declaring `pgsqlServers`/`pgsqlUsers` against a cluster whose pgsql
> protocol is *disabled* doesn't fail — the admin tables exist either way —
> but the config gets a `Degraded` condition with reason `PgsqlDisabled`,
> because it's almost certainly a mistake.

## 3. Query through port 6133 with psql

```sh
kubectl -n pg-tutorial run psql-client --rm -i --restart=Never --image=postgres:16 \
  --env=PGPASSWORD=pg-secret-pw -- \
  psql -h pgproxy -p 6133 -U app -d appdb -c "SELECT current_database(), version()"
```

```
 current_database |                                version
------------------+------------------------------------------------------------------------
 appdb            | PostgreSQL 16.14 (Debian 16.14-1.pgdg13+1) on x86_64-pc-linux-gnu, ...
(1 row)
```

Writes work the same way:

```sh
kubectl -n pg-tutorial run psql-client --rm -i --restart=Never --image=postgres:16 \
  --env=PGPASSWORD=pg-secret-pw -- \
  psql -h pgproxy -p 6133 -U app -d appdb \
    -c "CREATE TABLE IF NOT EXISTS notes (id SERIAL PRIMARY KEY, body TEXT)" \
    -c "INSERT INTO notes (body) VALUES ('hello over pgsql protocol')" \
    -c "SELECT * FROM notes"
```

```
CREATE TABLE
INSERT 0 1
 id |           body
----+---------------------------
  1 | hello over pgsql protocol
(1 row)
```

## 4. Inspect the runtime pgsql tables — with a MySQL client

This trips everyone up once: ProxySQL's **admin port (6032) always speaks
the MySQL wire protocol**, even on a PostgreSQL-only proxy. The `pgsql_*`
tables are admin tables; you query them with `mysql`, not `psql`:

```sh
RADMIN_PW="$(kubectl -n pg-tutorial get secret pgproxy -o jsonpath='{.data.radmin-password}' | base64 -d)"
kubectl -n pg-tutorial run admin-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD="$RADMIN_PW" -- \
  mysql -h pgproxy -P6032 -uradmin -e "SELECT hostgroup_id, hostname, port, status FROM runtime_pgsql_servers; SELECT username, default_hostgroup FROM runtime_pgsql_users; SELECT rule_id, match_pattern, destination_hostgroup FROM runtime_pgsql_query_rules"
```

```
hostgroup_id	hostname	port	status
0	postgres.pg-tutorial.svc.cluster.local	5432	ONLINE
username	default_hostgroup
app	0
app	0
rule_id	match_pattern	destination_hostgroup
100	^SELECT	0
```

Everything from the MySQL tutorials carries over: `pgsqlQueryRules` support
the same rewriting / caching / blocking / mirroring columns
([reference/proxysqlconfig.md](../reference/proxysqlconfig.md)), and
`pgsqlVariables` tunes the `pgsql-*` global variables.

## Clean up

```sh
kubectl delete namespace pg-tutorial
```

## Next

[Tutorial 4 — High availability](04-high-availability.md): scale to three
replicas, see write-to-all and `syncedReplicas` in action, and kill a pod to
watch the operator re-converge it.
