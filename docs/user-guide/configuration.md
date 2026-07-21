# Configuring ProxySQL declaratively

Everything about `ProxySQLConfig`: how the operator turns your YAML into
rows in ProxySQL's admin tables, how to express routing and rewriting
without surprises, and what happens on rotation, drift, and deletion.
For field-by-field documentation see the
[ProxySQLConfig reference](../reference/proxysqlconfig.md) and the
[admin-tables mapping](../reference/admin-tables.md).

## The write-to-all model

A `ProxySQLConfig` owns no Kubernetes objects. Reconciling it means: for
every *ready* pod of the referenced cluster, connect to the admin port
as `radmin` and, per section, `DELETE FROM <table>`, `INSERT` the
declared rows, then `LOAD ... TO RUNTIME; SAVE ... TO DISK`. The
operator writes to **every replica directly** — ProxySQL Cluster sync
also runs on multi-replica clusters as a backup (the operator keeps the
`proxysql_servers` peer table populated automatically; see the
[proxysqlServers reference](../reference/proxysqlconfig.md#proxysqlservers)),
but `status.syncedReplicas` counts the operator's own writes, so you
always know how many pods actually carry the config.

Because each sync **replaces** the managed tables wholesale, anything
you insert into those tables by hand on the admin port will be removed
at the next push or drift resync. The CR is the source of truth; treat
the admin port as read-only (see
[Operations](./operations.md#manual-admin-port-access)).

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: pxcfg
spec:
  clusterRef: {name: proxysql}   # same namespace, required
  ...
```

The target cluster must exist in the same namespace; otherwise the
config sits at `ClusterFound=False` and retries.

## Servers and users

```yaml
spec:
  mysqlServers:
    - {hostgroup: 0, hostname: mysql-primary.db.svc.cluster.local, port: 3306}
    - {hostgroup: 1, hostname: mysql-replicas.db.svc.cluster.local, port: 3306,
       weight: 5, maxConnections: 500, maxReplicationLag: 10}
  mysqlUsers:
    - username: app
      defaultHostgroup: 0
      defaultSchema: appdb
      passwordSecretRef: {name: app-creds, key: password}
```

User passwords are **never inline** — `passwordSecretRef` points at a
Secret key in the same namespace, which lets you reference the backend
operator's own credential Secret instead of copying passwords around
(the [cookbooks](./backends.md) all do this).

Two defaults worth knowing: declared users get `active=1` and
`transaction_persistent=1` unless you say otherwise, and schema-less
client sessions inherit ProxySQL's `mysql-default_schema`
(`information_schema`) — set `defaultSchema` if your backend's user
cannot open that as the handshake database.

**Password rotation:** the operator watches every Secret referenced by a
`passwordSecretRef` (and each target cluster's admin Secret). Updating
the Secret triggers a re-sync immediately — no CR touch, no waiting for
the resync interval. The user row is re-pushed with the new password and
survives the rotation.

PostgreSQL backends use the parallel `pgsqlServers` / `pgsqlUsers` /
`pgsqlQueryRules` fields — see
[Tutorial 03 — PostgreSQL](../tutorials/03-postgresql.md).

## Query rules, cookbook-style

Rules live in `mysqlQueryRules` (or `pgsqlQueryRules`) and map 1:1 to
ProxySQL's query-rule tables. The two semantics that bite everyone:

1. **Rules evaluate in `ruleId` order, and `apply: true` stops
   evaluation.** Put specific rules at low IDs and the catch-all router
   last, with gaps for future inserts.
2. **`replacePattern` substitutes only the text matched by
   `matchPattern`.** If the pattern matches a prefix, the unmatched
   remainder is appended to the replacement. Make the pattern consume
   everything you intend to replace (escape parentheses!).

```yaml
spec:
  mysqlQueryRules:
    # Rewriting: the pattern consumes the WHOLE call, escaped parens and
    # all — '^SELECT LEGACY_VERSION' alone would yield 'SELECT VERSION()()'.
    - ruleId: 90
      matchPattern: '^SELECT LEGACY_VERSION\(\)'
      replacePattern: 'SELECT VERSION()'
      destinationHostgroup: 0
      apply: true

    # Caching: serve this digest from the query cache for 5s.
    - ruleId: 95
      matchDigest: '^SELECT 42'
      destinationHostgroup: 0
      cacheTTL: 5000
      cacheEmptyResult: true
      apply: true

    # Routing: writes (and locking reads) to the writer hostgroup...
    - ruleId: 100
      matchDigest: '^SELECT.*FOR UPDATE$'
      destinationHostgroup: 0
      apply: true
    # ...then the catch-all read router LAST. With apply:true above,
    # a FOR UPDATE never reaches this rule.
    - ruleId: 200
      matchDigest: '^SELECT'
      destinationHostgroup: 1
      apply: true
```

`matchDigest` matches the normalized query digest (literals stripped) —
prefer it for routing. `matchPattern` matches the raw query text — use
it for rewriting. Other per-rule capabilities: `errorMessage` (block the
query, firewall-style), `mirrorHostgroup`, `timeout`, `delay`, `log`,
and chaining via `flagIn`/`flagOut`. Declared rules default to
`active: true` but `apply` defaults to **false** — an explicit
`apply: true` on terminal rules is almost always what you want.

**Duplicates are rejected at admission.** Every list is a
`listType=map` keyed on its identity (`ruleId`, `username`,
`hostgroup+hostname+port`), so a duplicate rule ID or username fails
`kubectl apply` — it never reaches the reconciler.

## Replication hostgroups and failover

```yaml
spec:
  mysqlServers:
    - {hostgroup: 0, hostname: db-0.db-headless.db.svc.cluster.local, port: 3306}
    - {hostgroup: 0, hostname: db-1.db-headless.db.svc.cluster.local, port: 3306}
  mysqlReplicationHostgroups:
    - {writerHostgroup: 0, readerHostgroup: 1, checkType: read_only}
```

ProxySQL's monitor polls each server's `read_only` flag and moves
servers between the writer and reader hostgroups accordingly — list all
nodes in the writer hostgroup and let the monitor sort them. This is the
supported failover mechanism: **the operator (and ProxySQL) follow
topology, they never manage it.** Whatever performs promotion —
a backend operator, Orchestrator, a managed-cloud control plane — flips
`read_only`, and ProxySQL follows within the monitor interval. A dead
primary with nothing promoted means the writer hostgroup drains and
writes fail, *correctly*: no proxy layer should invent a primary. The
full reasoning is in the
[external-failover design](../superpowers/specs/2026-06-10-external-failover-design.md).

Known caveat: the drift check currently keys servers by
`hostgroup:hostname:port`, so after a monitor-driven move the runtime
placement differs from your spec and the periodic resync re-pushes the
spec's static placement; the monitor re-corrects it within ~1.5s. Hold
this in mind when reading `status.driftedReplicas` on clusters using
replication hostgroups. This also requires the **monitor user** to exist
on the backends — see [Backends](./backends.md#the-monitor-user).

## Hostgroup attributes and variables

`mysqlHostgroupAttributes` tunes per-hostgroup connection behavior
(multiplexing, free-connection pool, init SQL, throttling):

```yaml
spec:
  mysqlHostgroupAttributes:
    - {hostgroup: 0, multiplex: false, freeConnectionsPct: 25}
```

Variables are plain string maps applied verbatim with
`UPDATE global_variables ... ; LOAD <domain> VARIABLES TO RUNTIME; SAVE
... TO DISK`. Keys are the full ProxySQL names, prefix included:

```yaml
spec:
  mysqlVariables:
    mysql-max_connections: "4096"
    mysql-monitor_read_only_interval: "1500"
  adminVariables:
    admin-refresh_interval: "2000"
```

Two consequences of "applied verbatim": there is no validation of names
or values beyond what ProxySQL itself enforces, and **removing a key
does not reset the variable** — ProxySQL has no "unset", so the last
written value persists until you write a new one.

## Raw SQL statements (escape hatch)

`sqlStatements` is a list of raw admin SQL, executed verbatim, in order, on
every ready replica — for anything the structured fields above don't model
(cache flushes, admin commands, settings not yet exposed as a CRD field):

```yaml
spec:
  clusterRef: {name: proxysql}
  sqlStatements:
    - "UPDATE global_variables SET variable_value='250' WHERE variable_name='mysql-max_connections'"
    - "LOAD MYSQL VARIABLES TO RUNTIME"
    - "PROXYSQL FLUSH QUERY CACHE"
```

Statements run **after** all structured config in the spec, and the
operator appends no implicit `LOAD`/`SAVE` — include those yourself if the
statement needs them.

> **Idempotency:** statements re-run on every sync pass, on new replicas,
> and after drift resyncs — write them so re-execution is harmless. This
> is desired-state SQL, not a one-shot migration script.

> **Lockout:** statements that change admin credentials will lock the
> operator out until a pod restart restores the cnf credentials.

A failing statement aborts the remaining statements on that replica and
surfaces through the usual `PartialSync`/`Degraded` conditions (see
[Operations](./operations.md#troubleshooting)). Statement text participates
in `status.lastAppliedHash`, but effects are **not** drift-tracked and are
**not** reversed by the deletion finalizer — see the
[field reference](../reference/proxysqlconfig.md#sqlstatements) for the
full contract.

## Deleting a ProxySQLConfig

Deletion is guarded by the `proxysql.com/config-cleanup` finalizer: the
operator first clears every managed table (servers, users, query rules —
runtime *and* disk) on each ready replica, then releases the CR. The
pods keep running; they just stop carrying that config. **Variables are
deliberately not reset** on deletion, for the no-unset reason above.
(Cleanup currently also clears the `proxysql_servers` peer table —
tracked as [#42](https://github.com/ProxySQL/proxysql-on-k8s/issues/42).)

The wedge policy is "never block deletion when cleanup is impossible"
(authoritative table in the
[annotations & finalizers reference](../reference/annotations.md#wedge-policy)):

| Situation | Behavior |
| --- | --- |
| Cluster gone / admin Secret gone or unusable | Release immediately — nothing can be cleaned. |
| Cluster exists, no ready pods | Hold and retry: releasing would leak config onto pods that come back. |
| Some replicas fail cleanup | Hold and retry until all ready replicas are clean. |
| Annotation `proxysql.com/skip-cleanup: "true"` | Skip cleanup, release. The escape hatch for any stuck deletion. |

```bash
kubectl annotate proxysqlconfig pxcfg proxysql.com/skip-cleanup=true
kubectl delete proxysqlconfig pxcfg
```

## Drift, resync, and reading status

After a successful sync the operator short-circuits when nothing
changed (same spec hash, same pod set, same generation). To stay
level-based it additionally performs an **informed resync** every two
minutes (operator flag `--config-resync-interval`, chart value
`configResyncInterval`): it reads the `runtime_*` tables back from each
replica — identities only, never passwords — and re-pushes **only the
replicas that actually drifted**. A converged cluster sees read-only
queries at steady state, not write churn. Out-of-band mutations to
managed tables are therefore healed within one resync interval.

What the status tells you (full details in the
[status reference](../reference/status.md)):

```bash
kubectl get pxcfg
NAME    CLUSTER    SYNCED   DRIFTED   LAST-SYNC   AGE
pxcfg   proxysql   3        0         12s         4d
```

- `syncedReplicas` — pods that received the latest config.
- `driftedReplicas` — ready pods whose runtime identities diverged at
  the last check (a failed read-back counts as drifted).
- `shunnedBackends` — backend rows in `SHUNNED` state across replicas.
  Shunning is ProxySQL's own health reaction (connect or monitor
  failures), **not** config drift — a shunned backend is present, just
  unhealthy. See [Operations](./operations.md#troubleshooting).
- `lastSyncTime` / `lastRuntimeCheckTime` — when state was last asserted
  / last verified.

## The pgsql-disabled warning

Declaring `pgsqlServers`/`pgsqlUsers`/`pgsqlQueryRules` against a
cluster whose `protocols.pgsql` is disabled still syncs (the admin
tables exist regardless), but sets `Degraded=True` with reason
`PgsqlDisabled` — it is almost certainly a mistake: clients have no
pgsql listener to connect to. Enable the protocol on the cluster or drop
the pgsql config.

## Next

- [Backends](./backends.md) — wiring real databases, cookbooks.
- [Tutorial 02 — query routing](../tutorials/02-query-routing.md).
- [Operations](./operations.md) — conditions, troubleshooting.
