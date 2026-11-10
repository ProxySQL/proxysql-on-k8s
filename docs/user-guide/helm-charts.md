# Helm charts: operations and classic configuration

This repo ships three Helm charts, all published at
`https://proxysql.github.io/proxysql-on-k8s`:

| Chart | Workload | Configuration model | Use it when |
| --- | --- | --- | --- |
| `proxysql-operator` | operator `Deployment` | `ProxySQLCluster` + `ProxySQLConfig` CRs, pushed to the admin port at runtime | Most installs. See [Running ProxySQL with the operator](running-proxysql.md) |
| `proxysql` | `Deployment`, stateless | Helm values rendered into `proxysql.cnf` (ConfigMap) | No operator allowed; a stateless data plane is enough |
| `proxysql-cluster` | `StatefulSet` + PVC per pod | Helm values rendered into `proxysql.cnf` (Secret), state kept in `proxysql.db` | No operator allowed; you want ProxySQL Cluster peers with persistent state |

The first half of this page covers operating the operator chart with
Helm. The second half covers the two standalone charts: "classic" ProxySQL
configuration, where servers, users, rules and variables live in Helm
values and reach ProxySQL through `proxysql.cnf`.

```bash
helm repo add proxysql https://proxysql.github.io/proxysql-on-k8s
helm repo update
helm search repo proxysql --versions     # every published chart version
```

## Operating the operator chart

### Install with a values file

Start from the chart's defaults for the exact version you install, keep
your overrides in a file, and pin `--version` so a later `helm repo update`
can't change what you deploy:

```bash
helm show values proxysql/proxysql-operator --version <chart-version> > operator-defaults.yaml
helm install proxysql-operator proxysql/proxysql-operator \
  -n proxysql-system --create-namespace \
  --version <chart-version> -f operator-values.yaml
```

The values you are most likely to set (full list in
[Helm values](../reference/helm-values.md)):

| Value | Default | What it does |
| --- | --- | --- |
| `replicaCount` | `1` | Manager replicas. Set `2` for operator HA; leader election keeps one active |
| `leaderElection.enabled` | `true` | Leave on whenever `replicaCount > 1` |
| `configResyncInterval` | `""` (= 2m) | How often the operator reads runtime tables back to detect drift |
| `image.repository` / `image.tag` | ghcr.io image / `""` (= appVersion) | Mirror into a private registry ([Installation](installation.md#images-and-private-registries)) |
| `resources`, `nodeSelector`, `tolerations`, `affinity` | see chart | Scheduling of the manager pod |
| `metrics.enabled` | `true` | The operator's own metrics endpoint on 8443 (not ProxySQL's 6070) |

### Inspect a release

```bash
helm list -n proxysql-system
helm get values proxysql-operator -n proxysql-system        # your overrides only
helm get values proxysql-operator -n proxysql-system --all  # merged with defaults
helm history proxysql-operator -n proxysql-system
```

### Upgrade

Helm never upgrades CRDs, so apply the CRDs for the target version first,
then upgrade pinned to the same version. Reuse your values file rather than
`--reuse-values`, so new defaults in the new chart apply:

```bash
helm repo update
kubectl apply -f https://raw.githubusercontent.com/ProxySQL/proxysql-on-k8s/proxysql-operator-<chart-version>/charts/proxysql-operator/crds/proxysqlcluster.yaml
kubectl apply -f https://raw.githubusercontent.com/ProxySQL/proxysql-on-k8s/proxysql-operator-<chart-version>/charts/proxysql-operator/crds/proxysqlconfig.yaml
helm upgrade proxysql-operator proxysql/proxysql-operator \
  -n proxysql-system --version <chart-version> -f operator-values.yaml
```

To see what an upgrade changes before applying it, render both sides with
`helm template` and diff them, or use the `helm-diff` plugin
(`helm diff upgrade ...`). ProxySQL pods keep serving while the operator
restarts.

### Roll back

```bash
helm rollback proxysql-operator <revision> -n proxysql-system
```

A rollback restores the manager Deployment, RBAC and values of that
revision. It does not touch CRDs: the newer CRDs you applied stay
installed.

### Uninstall

To remove only the operator, uninstall the release and leave the CRs
alone. ProxySQL pods keep serving with the config they hold, and a
reinstalled operator picks the CRs up again:

```bash
helm uninstall proxysql-operator -n proxysql-system
```

For a full teardown, delete the `ProxySQLConfig`s first, while the
operator still runs to remove their finalizers. **This is destructive:**
each deletion clears that config's servers, users and rules from the
running ProxySQL pods, so clients lose their backends. Delete them one
namespace at a time, never with `-A` on a shared cluster:

```bash
kubectl -n <namespace> delete proxysqlconfigs --all
helm uninstall proxysql-operator -n proxysql-system
```

Helm leaves the CRDs (and so every `ProxySQLCluster`) in place. Deleting a
CRD deletes all of its CRs and the objects they own; see
[Installation](installation.md) before you do.

## Classic configuration with the standalone charts

### How values map to proxysql.cnf

Each chart renders its values into one `proxysql.cnf`. Both charts share
the same values schema:

| Values key | proxysql.cnf section | Fields (defaults) |
| --- | --- | --- |
| `backends.mysql[]` / `backends.pgsql[]` | `mysql_servers` / `pgsql_servers` | `host` → `address`, `port` (3306 / 5432), `hostgroup` (0), `maxConnections` (1000), `comment` |
| `mysqlReplicationHostgroups[]` | `mysql_replication_hostgroups` | `writer`, `reader`, `checkType`, `comment` |
| `users.mysql[]` / `users.pgsql[]` | `mysql_users` / `pgsql_users` | `username`, `password` (inline), `defaultHostgroup` (0), `maxConnections` (mysql only); always `active=1` |
| `queryRules.mysql[]` / `queryRules.pgsql[]` | `mysql_query_rules` / `pgsql_query_rules` | `ruleId`, `active` (**false**), `matchPattern`, `matchDigest`, `destinationHostgroup`, `apply` (1) |
| `proxysqlServers[]` | `proxysql_servers` | `hostname`, `port` (6032), `weight` (0), `comment`. Non-empty also turns on the `admin-cluster_*` sync settings |
| `adminVariables`, `mysqlVariables`, `pgsqlVariables` | `admin_variables`, `mysql_variables`, `pgsql_variables` | Keys **without** the prefix: `threads`, not `mysql-threads` |
| `auth.adminPassword`, `radminPassword`, `monitorPassword` | `admin_credentials`, `monitor_password` | Blank = a random 32-character password, kept across upgrades |

Coming from the operator, four things differ:

| | Standalone charts | Operator (`ProxySQLConfig`) |
| --- | --- | --- |
| Query rule `active` when omitted | `0` (inactive): always set `active: true` | `1` |
| Variable keys | `mysqlVariables.threads` | `mysql-threads` (full name) |
| User passwords | Inline in values | `passwordSecretRef` |
| How a change applies | `helm upgrade` re-renders the cnf and rolls the pods | SQL pushed at runtime, no restart; drift detected and repaired |

Helm merges maps and replaces lists. Your `mysqlVariables` merge with the
chart's defaults (`threads: 4`, `max_connections: 2048`, monitor
intervals and so on), while `backends`, `users` and `queryRules` replace
the chart's empty lists outright.

### Sample: MySQL read/write split (`proxysql` chart)

One primary and two replicas. ProxySQL's monitor moves servers between
hostgroups 10 and 20 by `read_only`; locking reads go to the writer and
every other `SELECT` to the readers. Also in
[`examples/helm/proxysql-mysql-rw-split.yaml`](../../examples/helm/proxysql-mysql-rw-split.yaml).

```yaml
replicaCount: 2

backends:
  mysql:
    - { host: mysql-0.mysql.db.svc, port: 3306, hostgroup: 10, maxConnections: 2000, comment: primary }
    - { host: mysql-1.mysql.db.svc, port: 3306, hostgroup: 20, maxConnections: 2000, comment: replica }
    - { host: mysql-2.mysql.db.svc, port: 3306, hostgroup: 20, maxConnections: 2000, comment: replica }

mysqlReplicationHostgroups:
  - { writer: 10, reader: 20, checkType: read_only, comment: app-cluster }

users:
  mysql:
    - { username: app, password: change-me, defaultHostgroup: 10, maxConnections: 1000 }

queryRules:
  mysql:
    - { ruleId: 100, active: true, matchDigest: '^SELECT .* (FOR UPDATE|FOR SHARE|LOCK IN SHARE MODE)', destinationHostgroup: 10, apply: 1 }
    - { ruleId: 200, active: true, matchDigest: '^SELECT', destinationHostgroup: 20, apply: 1 }

mysqlVariables:
  threads: 4
  max_connections: 4096
  monitor_read_only_interval: 1500
  default_query_timeout: 36000000
```

```bash
helm install proxysql proxysql/proxysql -n app --create-namespace \
  --version <chart-version> -f proxysql-mysql-rw-split.yaml -f secrets.yaml
```

`secrets.yaml` holds the real `users.mysql` entries (and `auth.*` if you
set them) and stays out of git; values files later on the command line
win. Create the `monitor` user on the backends with the password from the
chart's Secret (see Connect and verify) so the read_only check can log in.

Keeping `secrets.yaml` out of git does not keep the passwords out of the
cluster: the `proxysql` chart renders the admin, monitor and application
passwords into `proxysql.cnf` in a **ConfigMap**, readable by anyone who
can read ConfigMaps in that namespace. Restrict that access with RBAC, or
use the `proxysql-cluster` chart, which keeps its cnf in a Secret.

### Sample: PostgreSQL (`proxysql` chart)

Writes to the primary Service, reads to the replicas Service. Also in
[`examples/helm/proxysql-pgsql.yaml`](../../examples/helm/proxysql-pgsql.yaml).

```yaml
protocols:
  mysql: { enabled: false }   # Service drops 6033; ProxySQL still binds it in the pod
  pgsql: { enabled: true, port: 6133 }

backends:
  pgsql:
    - { host: pg-rw.db.svc, port: 5432, hostgroup: 10, maxConnections: 500 }
    - { host: pg-ro.db.svc, port: 5432, hostgroup: 20, maxConnections: 500 }

users:
  pgsql:
    - { username: app, password: change-me, defaultHostgroup: 10 }

# auth.monitorPassword only reaches mysql_variables. The PostgreSQL monitor
# defaults to monitor/monitor: set an account that exists on the backends.
pgsqlVariables:
  monitor_username: monitor
  monitor_password: change-me

queryRules:
  pgsql:
    # Every row-locking clause, and sequence functions, must hit the primary:
    # a hot standby rejects both.
    - { ruleId: 100, active: true, matchDigest: '^SELECT .* FOR (UPDATE|NO KEY UPDATE|SHARE|KEY SHARE)', destinationHostgroup: 10, apply: 1 }
    - { ruleId: 150, active: true, matchDigest: '(nextval|setval|currval)\s*\(', destinationHostgroup: 10, apply: 1 }
    - { ruleId: 200, active: true, matchDigest: '^SELECT', destinationHostgroup: 20, apply: 1 }
```

Rule 200 sends every other `SELECT` to the standbys. A `SELECT` that calls
a function with side effects (writes, `pg_advisory_lock`, your own
volatile functions) needs its own writer rule ahead of it.

### Sample: three-node ProxySQL Cluster (`proxysql-cluster` chart)

Each pod lists all three peers so ProxySQL Cluster keeps them in sync. The
hostnames depend on the release name: install this as release
`proxysql-cluster`, or the chart names its objects
`<release>-proxysql-cluster` and every hostname below changes. Also in
[`examples/helm/proxysql-cluster-3-nodes.yaml`](../../examples/helm/proxysql-cluster-3-nodes.yaml).

```yaml
replicaCount: 3

proxysqlServers:
  - { hostname: proxysql-cluster-0.proxysql-cluster-headless.app.svc, port: 6032 }
  - { hostname: proxysql-cluster-1.proxysql-cluster-headless.app.svc, port: 6032 }
  - { hostname: proxysql-cluster-2.proxysql-cluster-headless.app.svc, port: 6032 }

persistence:
  enabled: true
  size: 1Gi
  storageClass: ""   # empty = the cluster's default StorageClass

backends:
  mysql:
    - { host: mysql-0.mysql.db.svc, port: 3306, hostgroup: 10 }
    - { host: mysql-1.mysql.db.svc, port: 3306, hostgroup: 20 }
mysqlReplicationHostgroups:
  - { writer: 10, reader: 20, checkType: read_only }
users:
  mysql:
    - { username: app, password: change-me, defaultHostgroup: 10 }
queryRules:
  mysql:
    - { ruleId: 100, active: true, matchDigest: '^SELECT .* (FOR UPDATE|FOR SHARE|LOCK IN SHARE MODE)', destinationHostgroup: 10, apply: 1 }
    - { ruleId: 200, active: true, matchDigest: '^SELECT', destinationHostgroup: 20, apply: 1 }
```

```bash
helm install proxysql-cluster proxysql/proxysql-cluster -n app \
  --version <chart-version> -f proxysql-cluster-3-nodes.yaml -f secrets.yaml
```

As with the first sample, create the `monitor` user on every backend with
the password from this release's Secret (`proxysql-cluster`, key
`monitor-password`), or set `auth.monitorPassword` in `secrets.yaml`.
Without it the read_only check fails and writer/reader placement stops
following promotions.

When you change `replicaCount`, change `proxysqlServers` to match in the
same upgrade.

## Operating the standalone charts

### Preview the rendered config

Render locally and read the cnf before installing or upgrading:

```bash
# proxysql chart: the cnf is in a ConfigMap
helm template proxysql proxysql/proxysql --version <chart-version> \
  -f proxysql-mysql-rw-split.yaml --show-only templates/configmap.yaml
# proxysql-cluster chart: the cnf is in a Secret (it carries the passwords)
helm template proxysql-cluster proxysql/proxysql-cluster --version <chart-version> \
  -f proxysql-cluster-3-nodes.yaml --show-only templates/secret-cnf.yaml
```

Blank `auth.*` passwords render as new random values on every
`helm template`. A real install keeps the first ones by reading the
existing Secret.

### Apply a change

Edit the values file and run `helm upgrade` with the same files. A
checksum of the rendered cnf sits on the pod template, so any config
change rolls the pods; ProxySQL reads the new cnf when each pod starts.

- **`proxysql` chart:** the datadir is an `emptyDir`, so every new pod
  starts from the cnf alone. Anything changed by hand on the admin port is
  gone once the pod is replaced (any `helm upgrade` that changes config
  replaces them). A container restart inside the same pod keeps the
  `emptyDir`, so whatever was saved to disk survives that.
- **`proxysql-cluster` chart (0.2.1 and later):** each pod keeps
  `proxysql.db` on its PVC and starts with `--reload`, which merges the cnf
  over it. Entries in the cnf win. **Entries you remove from values stay in
  `proxysql.db`**: deleting a backend, user or rule from values does not
  delete it from ProxySQL. Remove it on the admin port too (below), or
  start from an empty PVC. The reverse also holds: for a row that *is* in
  values, the cnf replaces the whole row, so columns the chart cannot
  express (for example `use_ssl` on `mysql_servers`) go back to their
  defaults on every pod start. Keep such rows out of values and manage them
  on the admin port, or use the operator.
- **`proxysql-cluster` before 0.2.1:** pods start without `--reload`, so
  ProxySQL ignores the cnf whenever `proxysql.db` already exists. A
  `helm upgrade` rolls the pods and changes nothing after the first boot.
  Upgrade the chart.

To remove a row on a `proxysql-cluster` release, delete it in values and
run `helm upgrade`. Wait for the rollout to finish
(`kubectl -n app rollout status sts/proxysql-cluster`), so no pod can
restart with the old cnf and bring the row back. Then delete it on the
admin port and save to disk; ProxySQL Cluster copies the change to the
other peers. Check `runtime_mysql_servers` on each pod afterwards:

```sql
DELETE FROM mysql_servers WHERE hostname = 'mysql-2.mysql.db.svc';
LOAD MYSQL SERVERS TO RUNTIME;
SAVE MYSQL SERVERS TO DISK;
```

### Connect and verify

Both charts create a Secret named after the release's full name (`proxysql`
and `proxysql-cluster` in the samples) with keys `admin-password`,
`radmin-password` and `monitor-password`. Use `radmin` for remote admin
sessions: ProxySQL accepts `admin` only from localhost.

Those Secret passwords match the ones in `proxysql.cnf` from `proxysql`
0.1.1 and `proxysql-cluster` 0.2.1. Earlier versions generated the two
separately on a fresh install with blank `auth.*`, so the Secret's
passwords were refused: set `auth.*Password` explicitly there.

```bash
helm test proxysql -n app                       # TCP check of the admin port
kubectl -n app port-forward svc/proxysql 6032:6032
```

In a second terminal:

```bash
RADMIN=$(kubectl -n app get secret proxysql -o jsonpath='{.data.radmin-password}' | base64 -d)
mysql -h127.0.0.1 -P6032 -uradmin -p"$RADMIN" \
  -e 'SELECT hostgroup_id, hostname, status FROM runtime_mysql_servers;
      SELECT rule_id, active, destination_hostgroup FROM runtime_mysql_query_rules;'
```

A rule that shows `active = 0` is missing `active: true` in values.

### Uninstall

```bash
helm uninstall proxysql -n app
helm uninstall proxysql-cluster -n app
```

`proxysql-cluster` leaves its PVCs (`data-proxysql-cluster-0`, `-1`,
`-2`) behind, and with them each pod's `proxysql.db`. Delete them to start
clean next time:

```bash
kubectl -n app delete pvc data-proxysql-cluster-0 data-proxysql-cluster-1 data-proxysql-cluster-2
```

## See also

- [Running ProxySQL with the operator](running-proxysql.md) for the CR-based path
- [Installation](installation.md) and the [Helm values reference](../reference/helm-values.md) for the operator chart
- [`charts/README.md`](../../charts/README.md) and each chart's `values.yaml` for every value
