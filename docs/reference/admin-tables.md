# ProxySQLConfig → admin table mapping

Exact mapping from every `ProxySQLConfig` field to the ProxySQL admin
table column it writes, including the SQL literal emitted when an optional
field is unset. This is the contract between the declarative API and what
you see when you `SELECT` from the admin port. API-level semantics live in
the [ProxySQLConfig reference](proxysqlconfig.md); conceptual guidance in
the [configuration user guide](../user-guide/configuration.md).

## The sync pattern

For every replica (connected as `radmin` on the admin port, MySQL wire
protocol), each table section is applied as:

```sql
DELETE FROM <table>;
INSERT INTO <table> (...) VALUES (...), (...);   -- one statement, all rows; skipped when the list is empty
LOAD <SECTION> TO RUNTIME;
SAVE <SECTION> TO DISK;
```

Properties:

- **Authoritative**: the DELETE runs unconditionally, so rows not in the
  spec are removed — including rows added by hand on the admin port. The
  managed tables are fully owned by the `ProxySQLConfig`.
- **Section independence**: if one section fails (e.g. `mysql_users`), the
  remaining sections are still attempted; previously applied sections stay
  applied. The first error is what surfaces in the `Degraded`/`SyncErrors`
  condition message (per-replica errors are aggregated).
- **Shared LOAD for the servers group**: `mysql_replication_hostgroups` and
  `mysql_hostgroup_attributes` are loaded to runtime together with
  `mysql_servers` — all three tables are written first, then a single
  `LOAD MYSQL SERVERS TO RUNTIME; SAVE MYSQL SERVERS TO DISK` applies them
  (verified live: runtime rows appear only after LOAD MYSQL SERVERS).
- Query-rule rows are inserted in ascending `rule_id` order (deterministic
  diffs); other lists keep spec order.
- Variables use `UPDATE global_variables` + `LOAD/SAVE <DOMAIN> VARIABLES`
  instead of DELETE/INSERT, and only run when the map is non-empty.

### LOAD/SAVE sections

| Section | Tables written before it |
|---|---|
| `MYSQL SERVERS` | `mysql_servers`, `mysql_replication_hostgroups`, `mysql_hostgroup_attributes` |
| `MYSQL USERS` | `mysql_users` |
| `MYSQL QUERY RULES` | `mysql_query_rules` |
| `PGSQL SERVERS` | `pgsql_servers` |
| `PGSQL USERS` | `pgsql_users` |
| `PGSQL QUERY RULES` | `pgsql_query_rules` |
| `PROXYSQL SERVERS` | `proxysql_servers` |
| `MYSQL VARIABLES` / `PGSQL VARIABLES` / `ADMIN VARIABLES` | `global_variables` (UPDATE per key; skipped entirely when the corresponding map is empty) |

### Value rendering

- Strings are emitted as SQL literals with single quotes doubled; NUL and C0
  control characters (except tab/newline/CR) are stripped as
  defense-in-depth.
- For NOT NULL columns, an unset field renders **ProxySQL's column default**
  (so unset behaves exactly like ProxySQL's own default).
- For genuinely nullable columns, an unset field renders **`NULL`** — never
  `''` or `0`, because ProxySQL treats them differently (e.g.
  `replace_pattern=''` rewrites queries to an empty string; any non-NULL
  `error_msg` blocks the query).

## Column maps

"SQL when unset" is the literal the operator emits for an absent/omitted
spec field.

### mysql_servers

| API field (`mysqlServers[]`) | Column | SQL when unset |
|---|---|---|
| `hostgroup` | `hostgroup_id` | required |
| `hostname` | `hostname` | required |
| `port` | `port` | `3306` |
| `weight` | `weight` | `1` |
| `maxConnections` | `max_connections` | `1000` |
| `maxReplicationLag` | `max_replication_lag` | `0` |
| `useSSL` | `use_ssl` | `0` |
| `comment` | `comment` | `''` |

### mysql_replication_hostgroups

| API field (`mysqlReplicationHostgroups[]`) | Column | SQL when unset |
|---|---|---|
| `writerHostgroup` | `writer_hostgroup` | required |
| `readerHostgroup` | `reader_hostgroup` | required |
| `checkType` | `check_type` | `'read_only'` |
| `comment` | `comment` | `''` |

### mysql_hostgroup_attributes

Every column is NOT NULL with a ProxySQL default; unset always renders the
column default, never NULL.

| API field (`mysqlHostgroupAttributes[]`) | Column | SQL when unset |
|---|---|---|
| `hostgroup` | `hostgroup_id` | required |
| `maxNumOnlineServers` | `max_num_online_servers` | `1000000` |
| `autocommit` | `autocommit` | `-1` (don't enforce) |
| `freeConnectionsPct` | `free_connections_pct` | `10` |
| `initConnect` | `init_connect` | `''` |
| `multiplex` | `multiplex` | `1` |
| `connectionWarming` | `connection_warming` | `0` |
| `throttleConnectionsPerSec` | `throttle_connections_per_sec` | `1000000` |
| `ignoreSessionVariables` | `ignore_session_variables` | `''` |
| `comment` | `comment` | `''` |

### mysql_users

| API field (`mysqlUsers[]`) | Column | SQL when unset |
|---|---|---|
| `username` | `username` | required |
| `passwordSecretRef` (resolved) | `password` | required (the resolved plaintext is pushed; it never appears in the CR or its status) |
| `defaultHostgroup` | `default_hostgroup` | `0` (CRD default) |
| `active` | `active` | `1` |
| `maxConnections` | `max_connections` | `10000` |
| `useSSL` | `use_ssl` | `0` |
| `defaultSchema` | `default_schema` | `''` (nullable column; `''` is fine here) |
| `transactionPersistent` | `transaction_persistent` | `1` |
| `comment` | `comment` | `''` |

### mysql_query_rules

| API field (`mysqlQueryRules[]`) | Column | SQL when unset | Unset meaning |
|---|---|---|---|
| `ruleId` | `rule_id` | required | — |
| `active` | `active` | `1` | a declared rule is active (operator choice; ProxySQL's own column default is 0) |
| `username` | `username` | `''` | match any user |
| `schemaName` | `schemaname` | `''` | match any schema |
| `flagIn` | `flagIN` | `0` | chain entry point |
| `matchPattern` | `match_pattern` | `''` | no raw-text match |
| `matchDigest` | `match_digest` | `''` | no digest match |
| `flagOut` | `flagOUT` | `NULL` | keep current flag |
| `replacePattern` | `replace_pattern` | `NULL` | no rewriting (`''` would rewrite to the empty query) |
| `destinationHostgroup` | `destination_hostgroup` | `NULL` | no routing override |
| `cacheTTL` | `cache_ttl` | `NULL` | no caching |
| `cacheEmptyResult` | `cache_empty_result` | `NULL` | inherit |
| `timeout` | `timeout` | `NULL` | `mysql-default_query_timeout` |
| `delay` | `delay` | `NULL` | no delay |
| `mirrorHostgroup` | `mirror_hostgroup` | `NULL` | no mirroring |
| `errorMessage` | `error_msg` | `NULL` | not blocked (any non-NULL value blocks) |
| `log` | `log` | `NULL` | inherit default logging behavior |
| `apply` | `apply` | `0` | continue evaluating later rules |
| `comment` | `comment` | `''` | — |

### pgsql_servers

| API field (`pgsqlServers[]`) | Column | SQL when unset |
|---|---|---|
| `hostgroup` | `hostgroup_id` | required |
| `hostname` | `hostname` | required |
| `port` | `port` | `5432` |
| `weight` | `weight` | `1` |
| `maxConnections` | `max_connections` | `1000` |
| `comment` | `comment` | `''` |

### pgsql_users

| API field (`pgsqlUsers[]`) | Column | SQL when unset |
|---|---|---|
| `username` | `username` | required |
| `passwordSecretRef` (resolved) | `password` | required |
| `defaultHostgroup` | `default_hostgroup` | `0` |
| `active` | `active` | `1` |
| `comment` | `comment` | `''` |

### pgsql_query_rules

Same column semantics as `mysql_query_rules`, minus `username`,
`schemaname`, and `match_digest` (not part of the API for pgsql rules):

| API field (`pgsqlQueryRules[]`) | Column | SQL when unset |
|---|---|---|
| `ruleId` | `rule_id` | required |
| `active` | `active` | `1` |
| `flagIn` | `flagIN` | `0` |
| `matchPattern` | `match_pattern` | `''` |
| `flagOut` | `flagOUT` | `NULL` |
| `replacePattern` | `replace_pattern` | `NULL` |
| `destinationHostgroup` | `destination_hostgroup` | `NULL` |
| `cacheTTL` | `cache_ttl` | `NULL` |
| `cacheEmptyResult` | `cache_empty_result` | `NULL` |
| `timeout` | `timeout` | `NULL` |
| `delay` | `delay` | `NULL` |
| `mirrorHostgroup` | `mirror_hostgroup` | `NULL` |
| `errorMessage` | `error_msg` | `NULL` |
| `log` | `log` | `NULL` |
| `apply` | `apply` | `0` |
| `comment` | `comment` | `''` |

### proxysql_servers

| API field (`proxysqlServers[]`) | Column | SQL when unset |
|---|---|---|
| `hostname` | `hostname` | required |
| `port` | `port` | `6032` |
| `weight` | `weight` | `0` |
| `comment` | `comment` | `''` |

An **empty** `proxysqlServers` list does *not* clear the peer table: when
the target cluster runs more than one replica, the operator auto-populates
the rows from the cluster's stable per-pod DNS names (admin port, weight 0,
comment `operator-populated from ProxySQLCluster pods`) before the table is
re-asserted; at `replicas ≤ 1` the table is left empty. Deletion cleanup
preserves the auto-populated peers the same way (an explicit
`proxysqlServers` list is cleared instead,
[#42](https://github.com/ProxySQL/proxysql-on-k8s/issues/42)) — see the
[proxysqlServers field notes](proxysqlconfig.md#proxysqlservers).

### global_variables

| API field | SQL emitted |
|---|---|
| `adminVariables["k"] = "v"` | `UPDATE global_variables SET variable_value='v' WHERE variable_name='k'` … then `LOAD ADMIN VARIABLES TO RUNTIME; SAVE ADMIN VARIABLES TO DISK` |
| `mysqlVariables` | same, `LOAD/SAVE MYSQL VARIABLES` |
| `pgsqlVariables` | same, `LOAD/SAVE PGSQL VARIABLES` |

An `UPDATE` against a `variable_name` that does not exist matches zero rows
and is silently a no-op; there is no INSERT/DELETE path and no way to
"unset" a variable back to its default.

## Drift-detection coverage

The periodic informed resync (see
[status.md](status.md#the-hash-short-circuit-and-informed-resync)) reads
back **identity keys only** from the `runtime_*` tables and compares:

| Table | In drift detection | Compared key |
|---|---|---|
| `runtime_mysql_servers` | yes | `hostgroup_id:hostname:port` — membership-aware: hostgroups joined by a `mysqlReplicationHostgroups` pair compare as one equivalence class, so a server in either hostgroup of its pair is present (status read but ignored for drift; counted for `shunnedBackends`) |
| `runtime_mysql_users` | yes | `username` (DISTINCT — runtime holds frontend + backend rows per user) |
| `runtime_mysql_query_rules` | yes | `rule_id` |
| `runtime_pgsql_servers` | yes | `hostgroup_id:hostname:port` |
| `runtime_pgsql_users` | yes | `username` (DISTINCT) |
| `runtime_pgsql_query_rules` | yes | `rule_id` |
| `mysql_replication_hostgroups` | **no** | loaded/saved with mysql_servers, so the realistic external mutation (a wiped servers table) is already caught |
| `mysql_hostgroup_attributes` | **no** | same reasoning |
| `proxysql_servers` | **no** | peer topology; re-asserted on every push (auto-populated when the spec list is empty) and self-healed by ProxySQL Cluster sync |
| `global_variables` | **no** | re-asserted on every actual push |

Notes:

- Comparison is **keys-only by design**: attribute-level changes (weight,
  comment, max_connections, a changed password…) do not register as runtime
  drift — they are caught by the spec-hash path instead, which fires
  whenever the spec or a referenced Secret changes.
- A `SHUNNED` server is *present*, therefore **not** drifted — shunning is
  ProxySQL's own health reaction, surfaced separately as
  `status.shunnedBackends`.
- Server comparison enforces **membership, not placement** (#34): within
  the hostgroups of a `mysqlReplicationHostgroups` pair, monitor-driven
  moves (`read_only` demotion, failover promotion, writer-is-also-reader
  mirroring) are not drift; a server missing from every hostgroup of its
  pair, or an unknown server, is. Pairs sharing a hostgroup chain into one
  equivalence class. Hostgroups outside every pair — and all
  `pgsql_servers` — keep exact-placement comparison.
- Passwords are never read back; the read-back queries select identity
  columns only.
- A replica whose read-back fails is treated as drifted (it cannot be proven
  converged) and goes back through the push path.
- Excluded tables are still **re-asserted** whenever any drift triggers a
  push, because Sync always writes every table.
