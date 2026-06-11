# ProxySQLConfig API reference

Complete field-by-field reference for the `ProxySQLConfig` custom resource
(`proxysql.com/v1alpha1`): the declarative ProxySQL configuration the
operator pushes to a target `ProxySQLCluster` over its admin port. Fields map
1:1 to ProxySQL admin tables; the exact column mapping and the SQL defaults
emitted for unset fields are in the [admin tables reference](admin-tables.md).
For task-oriented guidance see the
[configuration user guide](../user-guide/configuration.md) and the
[backends guide](../user-guide/backends.md).

| | |
|---|---|
| API group/version | `proxysql.com/v1alpha1` |
| Kind | `ProxySQLConfig` |
| Short name | `pxcfg` (`kubectl get pxcfg`) |
| Scope | Namespaced |
| Subresources | `status` |
| Printer columns | `Cluster`, `Synced`, `Drifted`, `Last-Sync`, `Age` |

A `ProxySQLConfig` owns **no** Kubernetes objects; reconciling it produces
SQL writes (DELETE/INSERT/LOAD/SAVE per table, UPDATE for variables) on every
ready replica of the referenced cluster, connecting as `radmin` (ProxySQL
restricts the `admin` account to localhost). A finalizer
(`proxysql.com/config-cleanup`) clears the managed tables on deletion — see
the [annotations & finalizers reference](annotations.md).

## List uniqueness keys

Every list field is `listType=map`, so the API server rejects duplicates at
admission (and server-side apply merges per-key):

| Field | Map keys |
|---|---|
| `mysqlServers` | `hostgroup`, `hostname`, `port` |
| `mysqlUsers` | `username` |
| `mysqlQueryRules` | `ruleId` |
| `mysqlReplicationHostgroups` | `writerHostgroup` |
| `mysqlHostgroupAttributes` | `hostgroup` |
| `pgsqlServers` | `hostgroup`, `hostname`, `port` |
| `pgsqlUsers` | `username` |
| `pgsqlQueryRules` | `ruleId` |
| `proxysqlServers` | `hostname`, `port` |

Note: because `port` participates in the server keys and is defaulted at
admission (3306/5432/6032), two entries differing only in an
explicit-vs-defaulted port are still distinct rows.

## Spec

### clusterRef

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `clusterRef.name` | `string` | — | required | Name of the target `ProxySQLCluster` in the **same namespace**. A missing cluster sets `ClusterFound=False` and retries every 5s. |

### mysqlServers

Maps to `mysql_servers`. CRD-level defaults below; unset optional fields are
emitted as the ProxySQL column default — see
[admin-tables.md](admin-tables.md#mysql_servers).

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `hostgroup` | `int32` | — | required (map key) | `hostgroup_id`. |
| `hostname` | `string` | — | required (map key) | Backend MySQL host. |
| `port` | `int32` | `3306` (CRD; sync also falls back to 3306) | map key | Backend port. |
| `weight` | `*int32` | unset → SQL `1` | — | Load-balancing weight. |
| `maxConnections` | `*int32` | unset → SQL `1000` | — | Per-server connection cap. |
| `maxReplicationLag` | `*int32` | unset → SQL `0` (disabled) | — | Shun the server when seconds-behind-master exceeds this. |
| `useSSL` | `*bool` | unset → SQL `0` | — | TLS to the backend. |
| `comment` | `string` | `''` | — | Free text. |

### mysqlUsers

Maps to `mysql_users`. Passwords are **never inline** — each entry references
a Secret key; the resolved password is pushed to ProxySQL and never written
back to the CR or status.

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `username` | `string` | — | required (map key) | Frontend/backend username. |
| `passwordSecretRef` | `corev1.SecretKeySelector` | — | required | Secret (same namespace) + key holding the password. A missing Secret/key sets `Ready=False`/`UserSecretError`. Changes to the referenced Secret trigger an immediate re-reconcile (Secret watch). |
| `defaultHostgroup` | `int32` | `0` (CRD) | — | Hostgroup for queries matching no rule. |
| `active` | `*bool` | unset → SQL `1` | — | Row active flag. |
| `maxConnections` | `*int32` | unset → SQL `10000` | — | Per-user frontend connection cap. |
| `useSSL` | `*bool` | unset → SQL `0` | — | Require TLS for this user. |
| `defaultSchema` | `string` | `''` | — | Default schema. |
| `transactionPersistent` | `*bool` | unset → SQL `1` | — | Pin a transaction to one hostgroup. |
| `comment` | `string` | `''` | — | Free text. |

### mysqlQueryRules

Maps to `mysql_query_rules`. Rules are inserted in ascending `ruleId` order.
For unset optional fields ProxySQL semantics depend on NULL vs `''` /
defaults — exact SQL in [admin-tables.md](admin-tables.md#mysql_query_rules).

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `ruleId` | `int32` | — | required (map key) | `rule_id`; also evaluation order. |
| `active` | `*bool` | unset → SQL `1` | — | A declared rule defaults to active (note: this differs from ProxySQL's own column default of 0). |
| `username` | `string` | unset → `''` | — | Match only this user's queries. |
| `schemaName` | `string` | unset → `''` | — | Match only this schema. |
| `matchPattern` | `string` | unset → `''` | — | Regex against the raw query text. |
| `matchDigest` | `string` | unset → `''` | — | Regex against the query digest. |
| `destinationHostgroup` | `*int32` | unset → SQL `NULL` (no routing override) | — | Route matching queries here. |
| `replacePattern` | `string` | unset → SQL `NULL` (no rewrite) | — | Replacement text for `matchPattern` matches (query rewriting; RE2/PCRE backreferences like `\1`). An empty string cannot be expressed: `""` renders as `NULL` because `replace_pattern=''` would rewrite queries to the empty string. |
| `mirrorHostgroup` | `*int32` | unset → SQL `NULL` (no mirroring) | min 0 | Also send a copy of matching queries here. |
| `timeout` | `*int32` | unset → SQL `NULL` (`mysql-default_query_timeout`) | min 0 | Kill matching queries running longer than this (ms). |
| `delay` | `*int32` | unset → SQL `NULL` (no delay) | min 0 | Throttle matching queries by this many ms. |
| `errorMessage` | `string` | unset → SQL `NULL` (not blocked) | — | Block matching queries and return this message (query firewalling). Any non-NULL value blocks — an empty-string message cannot be expressed (renders as `NULL`). |
| `flagIn` | `*int32` | unset → SQL `0` (chain entry point) | min 0 | Rule evaluated only when the query's current flag equals this. |
| `flagOut` | `*int32` | unset → SQL `NULL` (keep current flag) | min 0 | Flag used for subsequent rule evaluation on match (chaining). |
| `log` | `*bool` | unset → SQL `NULL` (inherit default) | — | Log matching queries. |
| `cacheTTL` | `*int32` | unset → SQL `NULL` (no caching) | min 0 | Cache matching resultsets for this many ms. |
| `cacheEmptyResult` | `*bool` | unset → SQL `NULL` | — | Cache empty resultsets too; only meaningful with `cacheTTL`. |
| `apply` | `*bool` | unset → SQL `0` | — | Stop evaluating further rules on match. |
| `comment` | `string` | `''` | — | Free text. |

### mysqlReplicationHostgroups

Maps to `mysql_replication_hostgroups` — automatic writer/reader placement
based on the backend's read-only state.

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `writerHostgroup` | `int32` | — | required (map key) | Hostgroup for writable servers. |
| `readerHostgroup` | `int32` | — | required | Hostgroup for read-only servers. |
| `checkType` | `string` | `read_only` (CRD; sync also falls back to `read_only`) | enum: `read_only`, `innodb_read_only`, `super_read_only`, `read_only\|innodb_read_only`, `read_only&innodb_read_only` | Which backend variable(s) the monitor checks. |
| `comment` | `string` | `''` | — | Free text. |

### mysqlHostgroupAttributes

Maps to `mysql_hostgroup_attributes` — per-hostgroup connection handling.
Every column is NOT NULL with a ProxySQL default; unset fields emit the
column default (shown in the Default column).

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `hostgroup` | `int32` | — | required (map key), min 0 | `hostgroup_id`. |
| `maxNumOnlineServers` | `*int32` | unset → SQL `1000000` | 0–1000000 | Cap on servers treated as ONLINE. |
| `autocommit` | `*int32` | unset → SQL `-1` | enum: -1, 0, 1 | Enforce autocommit on backend connections: -1 don't enforce, 0 force off, 1 force on. |
| `freeConnectionsPct` | `*int32` | unset → SQL `10` | 0–100 | % of `mysql-max_connections` kept as warm free connections. |
| `initConnect` | `string` | unset → SQL `''` | — | SQL run on every new backend connection (overrides `mysql-init_connect`). |
| `multiplex` | `*bool` | unset → SQL `1` | — | Connection multiplexing for the hostgroup. |
| `connectionWarming` | `*bool` | unset → SQL `0` | — | Pre-open free connections up to `freeConnectionsPct`. |
| `throttleConnectionsPerSec` | `*int32` | unset → SQL `1000000` | 1–1000000 | Cap new backend connections/sec. |
| `ignoreSessionVariables` | `string` | unset → SQL `''` | must be valid JSON or unset | JSON array of session variables ProxySQL must not track, e.g. `["sql_log_bin"]`. |
| `comment` | `string` | `''` | — | Free text. |

### pgsqlServers

Maps to `pgsql_servers` (ProxySQL 3.x).

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `hostgroup` | `int32` | — | required (map key) | `hostgroup_id`. |
| `hostname` | `string` | — | required (map key) | Backend PostgreSQL host. |
| `port` | `int32` | `5432` (CRD; sync also falls back to 5432) | map key | Backend port. |
| `weight` | `*int32` | unset → SQL `1` | — | Load-balancing weight. |
| `maxConnections` | `*int32` | unset → SQL `1000` | — | Per-server connection cap. |
| `comment` | `string` | `''` | — | Free text. |

Declaring any `pgsqlServers`/`pgsqlUsers`/`pgsqlQueryRules` against a cluster
whose `protocols.pgsql` is disabled still pushes the rows (the admin tables
exist either way) but raises `Degraded=True`/`PgsqlDisabled` on the config.

### pgsqlUsers

Maps to `pgsql_users`.

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `username` | `string` | — | required (map key) | Username. |
| `passwordSecretRef` | `corev1.SecretKeySelector` | — | required | Secret + key holding the password (same semantics as `mysqlUsers`). |
| `defaultHostgroup` | `int32` | `0` | — | Default hostgroup. |
| `active` | `*bool` | unset → SQL `1` | — | Row active flag. |
| `comment` | `string` | `''` | — | Free text. |

### pgsqlQueryRules

Maps to `pgsql_query_rules`. ProxySQL 3.x carries the same
rewriting/mirroring/caching/chaining columns as `mysql_query_rules`, so the
fields mirror [mysqlQueryRules](#mysqlqueryrules) minus `username`,
`schemaName`, and `matchDigest`:

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `ruleId` | `int32` | — | required (map key) | `rule_id`. |
| `active` | `*bool` | unset → SQL `1` | — | Declared rules default to active. |
| `matchPattern` | `string` | unset → `''` | — | Regex against the query text. |
| `destinationHostgroup` | `*int32` | unset → SQL `NULL` | — | Routing target. |
| `replacePattern` | `string` | unset → SQL `NULL` | — | Query rewriting (same NULL-vs-`''` semantics as MySQL rules). |
| `mirrorHostgroup` | `*int32` | unset → SQL `NULL` | min 0 | Query mirroring. |
| `timeout` | `*int32` | unset → SQL `NULL` | min 0 | Kill timeout (ms). |
| `delay` | `*int32` | unset → SQL `NULL` | min 0 | Throttle delay (ms). |
| `errorMessage` | `string` | unset → SQL `NULL` | — | Query firewalling. |
| `flagIn` | `*int32` | unset → SQL `0` | min 0 | Chaining entry flag. |
| `flagOut` | `*int32` | unset → SQL `NULL` | min 0 | Chaining exit flag. |
| `log` | `*bool` | unset → SQL `NULL` | — | Query logging. |
| `cacheTTL` | `*int32` | unset → SQL `NULL` | min 0 | Query cache TTL (ms). |
| `cacheEmptyResult` | `*bool` | unset → SQL `NULL` | — | Cache empty resultsets. |
| `apply` | `*bool` | unset → SQL `0` | — | Stop rule evaluation on match. |
| `comment` | `string` | `''` | — | Free text. |

### proxysqlServers

Maps to `proxysql_servers` — the peer list for ProxySQL Cluster sync.

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `hostname` | `string` | — | required (map key) | Peer hostname. |
| `port` | `int32` | `6032` (CRD; sync also falls back to 6032) | map key | Peer admin port. |
| `weight` | `int32` | `0` | — | Peer weight. |
| `comment` | `string` | `''` | — | Free text. |

In normal operation **leave this list empty** — two mechanisms keep the
peer table correct without it:

- When `replicas > 1`, the cluster's bootstrap `proxysql.cnf` seeds
  `proxysql_servers` with the stable per-pod DNS names
  (`<name>-<i>.<name>-headless.<ns>.svc:<adminPort>`) and the matching
  `cluster_*` admin variables (`cluster_username="radmin"`, check interval
  200ms, save-to-disk and diffs-before-sync settings for query rules /
  servers / users / proxysql_servers).
- On every config sync, an empty `proxysqlServers` is **auto-populated**
  from the same per-pod DNS names (admin port, `weight: 0`, comment
  `operator-populated from ProxySQLCluster pods`) before the table is
  re-asserted (`DELETE` + `INSERT` + `LOAD PROXYSQL SERVERS TO RUNTIME` +
  `SAVE ... TO DISK`) — so a sync never wipes the cnf-seeded peers and
  ProxySQL Cluster sync keeps operating alongside the operator's
  write-to-all pushes. When the defaulted replica count is ≤ 1 the table is
  left empty: there are no peers.

An explicitly non-empty list is passed through unchanged and fully replaces
the auto-populated peers — use it only for topologies the operator cannot
derive (e.g. peers outside this cluster).

Known limitation: *deleting* a `ProxySQLConfig` pushes an empty desired
state as cleanup, which currently clears the runtime peer table too —
tracked as [#42](https://github.com/ProxySQL/proxysql-on-k8s/issues/42).
The operator's direct write-to-all distribution is unaffected either way.

### Variables maps

| Field | Type | Default | Description |
|---|---|---|---|
| `adminVariables` | `map[string]string` | none | `UPDATE global_variables SET variable_value=<v> WHERE variable_name=<k>` per entry, then `LOAD ADMIN VARIABLES TO RUNTIME; SAVE ADMIN VARIABLES TO DISK`. |
| `mysqlVariables` | `map[string]string` | none | Same, with `LOAD/SAVE MYSQL VARIABLES`. |
| `pgsqlVariables` | `map[string]string` | none | Same, with `LOAD/SAVE PGSQL VARIABLES`. |

Variable semantics:

- Keys use ProxySQL's full variable names **including the prefix**
  (e.g. `mysql-max_connections`, `admin-refresh_interval`).
- An empty/absent map is a complete no-op: no UPDATE, no LOAD/SAVE —
  variables keep whatever ProxySQL was last told.
- There is no "unset": removing a key from the map stops asserting it but
  does **not** restore the ProxySQL default (this is also why deletion
  cleanup leaves variables untouched).
- Keys are applied in sorted order (deterministic logs/retries).

## Status

| Field | Type | Description |
|---|---|---|
| `observedGeneration` | `int64` | Last `.metadata.generation` the reconciler processed to completion of a push. |
| `lastAppliedHash` | `string` | SHA-256 fingerprint over the resolved desired state (passwords substituted) **and** the sorted set of ready pod addresses it was applied to. A pod recreated with a new IP changes the hash and forces a re-push. |
| `lastSyncTime` | `*metav1.Time` | When desired state was last **asserted** on the cluster — either written to all replicas, or verified converged via runtime read-back (an informed resync that finds zero drift also advances this). Drives the drift-resync clock. |
| `syncedReplicas` | `int32` | Number of ProxySQL pods carrying the latest config (all ready pods after a fully successful push; partial counts after `PartialSync`). |
| `driftedReplicas` | `int32` | Ready replicas whose runtime tables diverged from desired at the last runtime check (a failed read-back counts as drifted). 0 when converged. |
| `shunnedBackends` | `int32` | Total backend rows (MySQL + PostgreSQL) in `SHUNNED` runtime status across all replicas at the last runtime check. Shunned is ProxySQL's health reaction, **not** config drift. |
| `lastRuntimeCheckTime` | `*metav1.Time` | When runtime state was last read back from the replicas (only set by the informed-resync path). |
| `conditions` | `[]metav1.Condition` | `Ready`, `Progressing`, `Degraded`, `ClusterFound` — full reason inventory and requeue cadences in the [status reference](status.md). |
