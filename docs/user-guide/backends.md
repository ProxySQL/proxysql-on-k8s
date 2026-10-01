# Connecting backends

How to point ProxySQL at real databases: the ready-made cookbooks for
Kubernetes-native database operators, the pattern for external (non-K8s)
backends, and the requirements — monitor user, TLS, failover stance —
that apply to both. The mechanics of `mysqlServers` / `pgsqlServers` are
covered in [Configuration](./configuration.md); this page is about
choosing and wiring a topology.

## The cookbooks (Kubernetes-native backends)

[`examples/`](../../examples/README.md) ships six end-to-end recipes.
Each contains a `backend.yaml` (the database operator's own CR), a
`proxysql.yaml` (`ProxySQLCluster` + `ProxySQLConfig`), and a README
with install order and a smoke test. They are the fastest way to a
working stack — start from the one closest to your backend and edit the
server lists.

| Cookbook | Backend | One-liner |
| --- | --- | --- |
| [`mysql/oracle-mysql-operator/`](../../examples/mysql/oracle-mysql-operator/) | Oracle MySQL Operator (InnoDB Cluster) | ProxySQL replaces MySQL Router, targeting per-pod DNS of the instances. |
| [`mysql/percona-ps/`](../../examples/mysql/percona-ps/) | Percona Operator for MySQL Server (Group Replication) | Per-pod DNS + replication hostgroups; writer/reader split follows `read_only`. |
| [`mysql/percona-pxc/`](../../examples/mysql/percona-pxc/) | Percona Operator for PXC (Galera) | Multi-primary Galera behind the `<name>-pxc` Service. |
| [`mysql/mariadb-operator/`](../../examples/mysql/mariadb-operator/) | mariadb-operator (async replication) | `<name>-primary` / `<name>-secondary` Services as writer/reader hostgroups. |
| [`postgresql/cloudnativepg/`](../../examples/postgresql/cloudnativepg/) | CloudNativePG | `-rw` Service is always the primary (CNPG repoints it on failover), `-ro` the standbys. |
| [`postgresql/crunchy-pgo/`](../../examples/postgresql/crunchy-pgo/) | Crunchy PGO | `<name>-primary` / `<name>-replicas` Services. |

Plus shared load generators under
[`examples/loadgen/`](../../examples/loadgen/): a sysbench Job (MySQL,
port 6033) and a pgbench Job (PostgreSQL, port 6133).

Conventions the cookbooks share — worth keeping in your own configs:

- Hostgroup `0` = writer/primary, hostgroup `1` = reader pool.
- `mysqlUsers`/`pgsqlUsers` reference the backend operator's *own*
  credential Secret via `passwordSecretRef` — no copying passwords.
- One namespace per stack so several cookbooks can coexist.

Two distinct patterns appear in the server lists, pick deliberately:

- **Role Services** (CNPG `-rw`/`-ro`, MariaDB `-primary`): the backend
  operator already routes the Service to the current primary, so
  ProxySQL needs no failover handling at all — one hostname per
  hostgroup, done.
- **Per-pod DNS + replication hostgroups** (Percona PS, Oracle): list
  every node and let ProxySQL's monitor place each one by its
  `read_only` flag. Required when you want per-node weights, lag-based
  exclusion (`maxReplicationLag`), or there is no role Service.

## External (non-Kubernetes) backends

Managed cloud databases, VMs, bare-metal replication chains — anything
reachable only by address — work with the exact same `ProxySQLConfig`;
there is nothing Kubernetes-specific about a `hostname`:

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: external-mysql
spec:
  clusterRef: {name: proxysql}
  mysqlServers:
    # List every node in the WRITER hostgroup; the monitor demotes the
    # read_only ones to the reader hostgroup within ~1.5s.
    - {hostgroup: 0, hostname: db-1.example.internal, port: 3306, useSSL: true}
    - {hostgroup: 0, hostname: db-2.example.internal, port: 3306, useSSL: true}
    - {hostgroup: 0, hostname: db-3.example.internal, port: 3306, useSSL: true,
       maxReplicationLag: 10}
  mysqlReplicationHostgroups:
    - {writerHostgroup: 0, readerHostgroup: 1, checkType: read_only}
  mysqlUsers:
    - username: app
      defaultHostgroup: 0
      passwordSecretRef: {name: external-db-creds, key: app-password}
  mysqlVariables:
    # Tune how fast ProxySQL follows a read_only flip (milliseconds).
    mysql-monitor_read_only_interval: "1500"
    mysql-monitor_read_only_timeout: "500"
```

### The failover stance: follow, never manage

For external backends, **ProxySQL-native replication hostgroups +
`read_only` monitoring is the supported failover mechanism** — and the
operator's role ends there. Whoever is the HA authority (the cloud
provider's control plane, Orchestrator/MHA, your platform's promotion
scripts, Group Replication election) flips `read_only` or repoints an
endpoint; ProxySQL follows within the monitor interval. The operator
never probes topology and never promotes — a dead primary with nothing
promoted means the writer hostgroup drains and writes fail *correctly*,
because two promotion authorities racing each other is how split-brain
happens. If your backends have no HA authority at all, that is an
availability problem the proxy layer cannot fix: run one. Full
trade-off analysis in the
[external-failover design decision](../superpowers/specs/2026-06-10-external-failover-design.md).

`checkType` selects what the monitor polls: `read_only` (default),
`innodb_read_only`, `super_read_only`, or the `|`/`&` combinations.

### Backend requirements checklist

- **Network reachability** from the pod network to the backend
  addresses/ports (VPC peering, firewall rules for the cluster's egress).
- **The monitor user** (next section).
- **Frontend users exist on the backend** with the same username and
  password that the `passwordSecretRef` resolves to — ProxySQL
  authenticates the client itself, then opens backend connections with
  the same credentials.
- **TLS to the backend** where required: `useSSL: true` per
  `mysqlServers` / `pgsqlServers` entry.

## The monitor user

ProxySQL's monitor module logs into every backend to run connect, ping,
and `read_only` checks. The bootstrap cnf configures it as user
`monitor` with the `monitor-password` from the cluster's auth Secret.
A single failed check does not shun a backend: ProxySQL requires
consecutive failures (`mysql-monitor_ping_max_failures` /
`pgsql-monitor_ping_max_failures`, default 3) before it marks an
otherwise-healthy server SHUNNED — brief monitor blips are absorbed.
Three ways to make it work, in order of preference:

1. **Create the user on the backends** with the operator-minted
   password:

   ```bash
   MONPW=$(kubectl get secret proxysql \
     -o jsonpath='{.data.monitor-password}' | base64 -d)
   # On the primary:
   #   CREATE USER 'monitor'@'%' IDENTIFIED BY '<MONPW>';
   #   GRANT USAGE, REPLICATION CLIENT ON *.* TO 'monitor'@'%';
   ```

2. **Point ProxySQL at an existing backend user** by overriding the
   monitor variables in the `ProxySQLConfig` (these are plain strings —
   keep them in sync with the backend's secret yourself):

   ```yaml
   mysqlVariables:
     mysql-monitor_username: "monitor"
     mysql-monitor_password: "<the backend's monitor password>"
   ```

3. **Disable the monitor** (`mysql-monitor_enabled: "false"`) — only
   sensible without replication hostgroups, e.g. a single backend behind
   a role Service.

A misconfigured monitor is the classic silent failure: backends get
**SHUNNED** despite being perfectly healthy, and
`ProxySQLConfig.status.shunnedBackends` climbs. Diagnosis steps in
[Operations](./operations.md#troubleshooting).

## Galera clusters

Galera-based backends — Galera itself, Percona XtraDB Cluster, MariaDB
Cluster — have no `read_only` writer flag to follow, so
`mysqlReplicationHostgroups` does not apply to them.
`mysqlGaleraHostgroups` does: one row wires **four** hostgroups together
and ProxySQL's Galera monitor places each node among them from its wsrep
state.

```yaml
spec:
  # Declare every node in the WRITER hostgroup. The monitor does the rest.
  mysqlServers:
    - {hostgroup: 10, hostname: pxc-0.pxc, port: 3306}
    - {hostgroup: 10, hostname: pxc-1.pxc, port: 3306}
    - {hostgroup: 10, hostname: pxc-2.pxc, port: 3306}
  mysqlGaleraHostgroups:
    - writerHostgroup: 10
      backupWriterHostgroup: 12
      readerHostgroup: 11
      offlineHostgroup: 13
      maxWriters: 1          # single-writer routing
      writerIsAlsoReader: 2  # only the backup writers serve reads
```

- **writer** holds up to `maxWriters` nodes. `maxWriters: 1` routes all
  writes to one node, which avoids Galera certification conflicts —
  concurrent conflicting transactions on different nodes fail
  certification at `COMMIT`. Raise it for multi-writer.
- **backup writer** holds the remaining Synced nodes, promoted when the
  writer leaves.
- **reader** holds any healthy node reporting `read_only=1`, plus whichever
  writable tiers `writerIsAlsoReader` mirrors in: `0` neither, `1` writers and
  backup writers, `2` only backup writers. With `0` the hostgroup is therefore
  empty only while no node is read-only.
- **offline** is where the monitor parks a node that is not Synced, is
  desynced, has `wsrep_reject_queries` set, or has exceeded
  `maxTransactionsBehind` (its `wsrep_local_recv_queue` backlog).

The [follow-never-manage stance](#the-failover-stance-follow-never-manage)
applies in full: the operator syncs the table and tolerates the monitor's
moves. It never probes a node's wsrep state itself, never promotes and
never writes to a backend. Field-by-field details are in the
[`mysqlGaleraHostgroups` reference](../reference/proxysqlconfig.md#mysqlgalerahostgroups);
`examples/mysql/percona-pxc/` is a working cookbook entry.

Group replication (`mysql_group_replication_hostgroups`) and Aurora are
not modelled yet — use `sqlStatements` for those, with the drift caveat
below.

## Drift detection and hostgroup topologies

The operator's drift detection enforces **membership, not placement**.
For every hostgroup covered by a declared topology, a listed server
counts as converged when runtime holds it in *any* hostgroup of that
topology's equivalence class:

| Declared by | Equivalence class |
|---|---|
| `mysqlReplicationHostgroups` row | writer + reader |
| `mysqlGaleraHostgroups` row | writer + backup writer + reader + offline |

So the read-only monitor demoting a writer on a `read_only` flip,
promoting a replica during failover, or mirroring the writer into the
reader hostgroup (`mysql-monitor_writer_is_also_reader`) is ProxySQL
doing its job, never drift — and equally, the Galera monitor electing a
writer under `maxWriters`, mirroring backup writers into the reader
hostgroup, or parking an unusable node in the offline hostgroup. A node
in the offline hostgroup is still a declared member of its topology, not
drift. The same goes for health status: a `SHUNNED` backend is present,
not drifted. What *is* drift: a listed server missing from every
hostgroup of its class, or an unknown server appearing in one — both
trigger a re-push. Topologies sharing a hostgroup chain into one class.
Hostgroups not covered by any pair or row keep exact placement
enforcement (with nothing declared, nothing may legitimately move a
server), and `pgsqlServers` are always exact — this operator exposes no
PostgreSQL cluster-hostgroup field. If you configure pgsql replication
hostgroups out-of-band via `sqlStatements`, pgsql drift detection will
fight the monitor's placement moves exactly as described in
[#34](https://github.com/ProxySQL/proxysql-on-k8s/issues/34) — don't. The
same warning applied to Galera hostgroups pushed through `sqlStatements`
before `mysqlGaleraHostgroups` existed.

One transient to know about: when a re-push *does* happen on a config
with replication or Galera hostgroups — a spec change, or healing real
drift — the full table write momentarily resets servers to the spec's
static placement. The monitor re-places them within one
`mysql-monitor_read_only_interval` (1.5 s by default), or one
`mysql-monitor_galera_healthcheck_interval` (5 s by default) for Galera.
That window only opens on actual changes, not on the periodic resync of a
converged cluster.

## What's coming: backend auto-discovery

For Kubernetes-native backends, watching the backend operator's CR
status and mapping roles to hostgroups automatically — no hand-written
server lists — is on the roadmap as
[backend auto-discovery (#22)](https://github.com/ProxySQL/proxysql-on-k8s/issues/22),
with a design sketch in
[`docs/superpowers/specs/2026-06-10-backend-autodiscovery-design.md`](../superpowers/specs/2026-06-10-backend-autodiscovery-design.md).
Roadmap, not promise: everything on this page works without it, and the
explicit `mysqlServers`/`pgsqlServers` lists remain fully supported.

## Next

- [Tutorial 01 — first cluster](../tutorials/01-first-cluster.md) and
  [Tutorial 03 — PostgreSQL](../tutorials/03-postgresql.md).
- [Configuration](./configuration.md) — query routing on top of these
  backends.
