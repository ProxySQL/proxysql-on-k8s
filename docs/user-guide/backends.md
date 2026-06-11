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
  `mysqlServers` entry (MySQL protocol only — `pgsqlServers` has no
  `useSSL` field today).

## The monitor user

ProxySQL's monitor module logs into every backend to run connect, ping,
and `read_only` checks. The bootstrap cnf configures it as user
`monitor` with the `monitor-password` from the cluster's auth Secret.
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

## Known limitation with replication hostgroups

The operator's drift detection currently keys servers by
`hostgroup:hostname:port`. After the monitor moves a server (writer
demoted to the reader hostgroup), runtime no longer matches the spec's
static placement, so each periodic resync re-pushes the spec placement
and the monitor re-corrects it within one `read_only` interval. The
visible effects: recurring `driftedReplicas` on such configs, and a
sub-2-second window after each resync where a write could hit a
now-read-only server and fail. Making drift detection
replication-hostgroup-aware is a planned follow-up of the
[failover design](../superpowers/specs/2026-06-10-external-failover-design.md).
Until then: where a role Service exists (pattern 1 above), prefer it.

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
