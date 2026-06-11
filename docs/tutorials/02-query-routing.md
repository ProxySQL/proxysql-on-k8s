# Tutorial 2 — Query routing: users and rules

**What you'll learn**

- Routing queries to different hostgroups with `mysqlQueryRules`
- Why rule order matters and what `apply: true` does
- Rewriting queries in flight (and the full-match gotcha)
- Caching resultsets with `cacheTTL`
- Verifying rules via `runtime_mysql_query_rules` and live traffic

**Prerequisites**

- [Tutorial 1](01-first-cluster.md) completed, with the `proxysql-tutorial`
  namespace still around (operator installed, `proxysql` cluster + config,
  `mysql` backend, `app-user` Secret).

## 1. Add a second backend

Routing is only interesting with somewhere to route *to*. Deploy a second,
independent MySQL to play the "reader" role:

```sh
kubectl -n proxysql-tutorial apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mysql-reader
spec:
  replicas: 1
  selector:
    matchLabels: {app: mysql-reader}
  template:
    metadata:
      labels: {app: mysql-reader}
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
  name: mysql-reader
spec:
  selector: {app: mysql-reader}
  ports: [{port: 3306}]
EOF
kubectl -n proxysql-tutorial rollout status deploy/mysql-reader --timeout=180s
```

> [!WARNING]
> These are two *independent* servers — there is no replication between
> them, so data written to one does not appear on the other. Good enough to
> watch routing happen; for a real read/write split you'd point the
> hostgroups at a primary and its replicas
> ([user-guide/backends.md](../user-guide/backends.md)).

## 2. Route SELECTs to the reader hostgroup

Update the `ProxySQLConfig`: the reader joins as **hostgroup 10**, and one
query rule sends every `SELECT` there. Everything else falls through to the
user's `defaultHostgroup` (0 — the writer).

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
    - hostgroup: 10
      hostname: mysql-reader.proxysql-tutorial.svc.cluster.local
      port: 3306
  mysqlUsers:
    - username: app
      defaultHostgroup: 0
      defaultSchema: appdb
      passwordSecretRef: {name: app-user, key: password}
  mysqlQueryRules:
    - ruleId: 100
      active: true
      matchDigest: "^SELECT"
      destinationHostgroup: 10
      apply: true
  mysqlVariables:
    mysql-monitor_enabled: "false"
EOF
kubectl -n proxysql-tutorial get pxcfg proxysql
```

```
NAME       CLUSTER    SYNCED   DRIFTED   LAST-SYNC   AGE
proxysql   proxysql   1                  8s          93s
```

(`LAST-SYNC` a few seconds old = the update has been pushed.) How the rule
matches: **`matchDigest`** runs the regex against the query's *digest* — the
normalized text with literals stripped (`SELECT ?`) — which is cheap and
stable across parameter values. **`matchPattern`** (used below) matches the
raw SQL text instead, which is what you need for rewrites.

## 3. Watch routing happen

A `SELECT` now lands on the reader — `@@hostname` tells you which server
actually answered:

```sh
kubectl -n proxysql-tutorial run mysql-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD=app-secret-pw -- \
  mysql -h proxysql -P6033 -uapp -e "SELECT @@hostname"
```

```
@@hostname
mysql-reader-f5bb4b659-kbwpd
```

A write matches no rule, so it goes to `defaultHostgroup` 0:

```sh
kubectl -n proxysql-tutorial run mysql-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD=app-secret-pw -- \
  mysql -h proxysql -P6033 -uapp -e "INSERT INTO greetings (msg) VALUES ('routed to the writer')"
```

ProxySQL keeps per-hostgroup counters — ask the admin port:

```sh
RADMIN_PW="$(kubectl -n proxysql-tutorial get secret proxysql -o jsonpath='{.data.radmin-password}' | base64 -d)"
kubectl -n proxysql-tutorial run admin-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD="$RADMIN_PW" -- \
  mysql -h proxysql -P6032 -uradmin -e "SELECT hostgroup, srv_host, Queries FROM stats_mysql_connection_pool"
```

```
hostgroup	srv_host	Queries
0	mysql.proxysql-tutorial.svc.cluster.local	4
10	mysql-reader.proxysql-tutorial.svc.cluster.local	1
```

(Your absolute counts will differ — what matters is that both hostgroups are
taking the right kind of traffic.)

## 4. Rule order, and why `apply: true` matters

Rules are evaluated in ascending `ruleId` order, and a matching rule with
`apply: true` **stops evaluation** — nothing after it runs for that query.
Two consequences:

1. **Specific rules need lower `ruleId`s than catch-alls.** If
   `^SELECT → hostgroup 10` ran first with `apply: true`, a later
   "cache `SELECT NOW()`" rule would never be reached, because `SELECT NOW()`
   also matches `^SELECT`.
2. **`apply: false` (the default) lets matching continue** — used for rule
   chains (`flagOut`/`flagIn`) where one rule tags a query and later rules
   refine it.

So the layout below puts the rewrite rule at 90 and the cache rule at 95,
*before* the catch-all router at 100:

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
    - hostgroup: 10
      hostname: mysql-reader.proxysql-tutorial.svc.cluster.local
      port: 3306
  mysqlUsers:
    - username: app
      defaultHostgroup: 0
      defaultSchema: appdb
      passwordSecretRef: {name: app-user, key: password}
  mysqlQueryRules:
    # Rewrite: legacy SQL fixed in flight. The pattern must consume the
    # WHOLE text being replaced — see the gotcha below.
    - ruleId: 90
      active: true
      matchPattern: '^SELECT LEGACY_VERSION\(\)'
      replacePattern: 'SELECT VERSION()'
      destinationHostgroup: 10
      apply: true
    # Cache: resultsets for this digest are served from ProxySQL's query
    # cache for 5 seconds.
    - ruleId: 95
      active: true
      matchDigest: '^SELECT NOW\(\)'
      destinationHostgroup: 10
      cacheTTL: 5000
      apply: true
    # Catch-all read router: every other SELECT goes to the reader.
    - ruleId: 100
      active: true
      matchDigest: "^SELECT"
      destinationHostgroup: 10
      apply: true
  mysqlVariables:
    mysql-monitor_enabled: "false"
EOF
```

## 5. The rewrite rule (and the full-match gotcha)

`SELECT LEGACY_VERSION()` doesn't exist on any MySQL — but rule 90 rewrites
it before the backend ever sees it:

```sh
kubectl -n proxysql-tutorial run mysql-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD=app-secret-pw -- \
  mysql -h proxysql -P6033 -uapp -e "SELECT LEGACY_VERSION()"
```

```
VERSION()
8.0.46
```

Even the column header says `VERSION()` — the backend received the rewritten
text.

> [!WARNING]
> **The full-match gotcha.** `replacePattern` substitutes *the text matched
> by the regex*, not the whole query. If the pattern were just
> `LEGACY_VERSION`, the result would be `SELECT SELECT VERSION()()` — the
> unmatched `()` survives and gets glued on. The pattern must consume
> everything you want replaced, which is why the parentheses are escaped and
> included: `^SELECT LEGACY_VERSION\(\)`.

## 6. The cache rule

Rule 95 caches `SELECT NOW()` resultsets for 5 seconds (`cacheTTL` is in
milliseconds). `NOW()` normally changes every second — so two identical
answers 2 seconds apart prove the second one came from ProxySQL's cache, not
the database:

```sh
kubectl -n proxysql-tutorial run mysql-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD=app-secret-pw -- \
  mysql -h proxysql -P6033 -uapp -e "SELECT NOW(); DO SLEEP(2); SELECT NOW()"
```

```
NOW()
2026-06-11 09:50:18
NOW()
2026-06-11 09:50:18
```

## 7. Verify via the runtime tables

```sh
RADMIN_PW="$(kubectl -n proxysql-tutorial get secret proxysql -o jsonpath='{.data.radmin-password}' | base64 -d)"
kubectl -n proxysql-tutorial run admin-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD="$RADMIN_PW" -- \
  mysql -h proxysql -P6032 -uradmin -e "SELECT rule_id, match_digest, match_pattern, replace_pattern, cache_ttl, destination_hostgroup, apply FROM runtime_mysql_query_rules ORDER BY rule_id; SELECT rule_id, hits FROM stats_mysql_query_rules ORDER BY rule_id"
```

```
rule_id	match_digest	match_pattern	replace_pattern	cache_ttl	destination_hostgroup	apply
90		^SELECT LEGACY_VERSION\\(\\)	SELECT VERSION()	NULL	10	1
95	^SELECT NOW\\(\\)		NULL	5000	10	1
100	^SELECT		NULL	NULL	10	1
rule_id	hits
90	1
95	2
100	0
```

The `hits` counters confirm the ordering lesson: the rewrite fired once, the
cache rule caught both `NOW()` calls, and neither ever fell through to rule
100 (`apply: true` stopped them). Hit counters reset whenever rules are
reloaded — i.e. on every config sync.

Query rules can do much more (mirroring, blocking with `errorMessage`,
timeouts, chaining); see
[user-guide/configuration.md](../user-guide/configuration.md) and the field
reference in [reference/proxysqlconfig.md](../reference/proxysqlconfig.md).

## Clean up

**Continuing to [tutorial 4](04-high-availability.md) or
[5](05-query-logging.md)? Keep the namespace** — they build on it.
([Tutorial 3](03-postgresql.md) uses its own namespace either way.)

```sh
kubectl delete namespace proxysql-tutorial
```

## Next

[Tutorial 3 — PostgreSQL through ProxySQL](03-postgresql.md): the same
operator and CRDs, speaking PostgreSQL on port 6133.
