# External-Backend Failover: Design Decision

**Date:** 2026-06-10
**Status:** Decided
**Issue:** #30 (design-only; outcome feeds the scope of #22)
**Decision:** The operator follows topology, it never manages it. ProxySQL-native
`mysql_replication_hostgroups` + read_only monitoring is the supported failover
mechanism for external backends that have their own HA authority. Promotion
authority is explicitly out of scope.

## Context

Backend auto-discovery (#22) covers databases managed by in-cluster operators
(CloudNativePG, MariaDB Operator, PXC, …): those backends publish topology in CR
status, and their operators *perform* failover. This decision covers the other
case — backends reachable only by address (managed cloud databases, VMs,
bare-metal replication chains run by a platform/control-plane outside the
cluster). When the writer dies or moves there, who updates ProxySQL's routing?

What the operator already does today, relevant here:

- `ProxySQLConfig.spec.mysqlReplicationHostgroups` syncs to ProxySQL's
  `mysql_replication_hostgroups` table, loaded atomically with `mysql_servers`
  in one `LOAD MYSQL SERVERS TO RUNTIME` (`internal/proxysqlclient/sync.go`).
- `checkType` is admission-validated to the full enum ProxySQL accepts
  (`read_only`, `innodb_read_only`, `super_read_only`, and the OR/AND combos).
- Monitor credentials are minted per cluster and rendered into the bootstrap
  cnf, so the monitor module works out of the box once the same user exists on
  the backends.

## Options

### Option 1 — ProxySQL-native: replication hostgroups + read_only monitoring

ProxySQL's monitor module polls each server's `read_only` flag (default every
1.5s, `mysql-monitor_read_only_interval`). When a server in the writer
hostgroup reports `read_only=1` it is moved to the reader hostgroup; when a
reader reports `read_only=0` it is promoted into the writer hostgroup. A writer
whose read_only check times out repeatedly is taken out of service. Combined
with shunning on connect failures, this gives sub-2-second *follow* of any
failover that manifests as a read_only flip — which is exactly what every
serious MySQL HA system produces: managed-cloud failover (endpoint repoints or
replica flips), Orchestrator/MHA promotion, Group Replication primary election,
or a platform's own promotion scripts.

What it does **not** cover: a dead primary with no surviving server flipping
`read_only=0`. ProxySQL shuns the dead writer, the writer hostgroup drains, and
writes fail — correctly, because nothing has been promoted. ProxySQL has no
promotion authority and never issues `SET GLOBAL read_only=0` or `CHANGE
REPLICATION SOURCE`. It *follows* topology; it cannot change it. That is a
feature: a proxy layer (often several independent ProxySQL deployments) must
never race the actual HA authority on who the primary is.

One real wrinkle found while grounding this design: the operator's drift
read-back (`Desired.Drift`) keys servers by `hostgroup:hostname:port`. After a
monitor-driven move (writer now in the reader hostgroup), runtime no longer
matches the spec's static placement, so every resync interval flags drift and
re-pushes the stale placement. ProxySQL's monitor corrects it within one
read_only interval (~1.5s), but during that window writes can be routed to a
read_only server and fail. The fix is small and listed under "What changes".

### Option 2 — External topology manager (Orchestrator-style)

Deploy and manage a topology manager seeded with discovery hosts; it detects
failure, performs promotion, and (via hooks) rewrites `mysql_servers`. This is
the only option that adds genuine promotion authority. The cost: the operator
would own the lifecycle of a heavyweight stateful dependency (its own backing
store, quorum/raft for anti-flapping, fencing semantics), and would carry
responsibility for *correct promotion* of databases it did not provision — a
data-loss-grade liability. It also overlaps badly with the K8s-native backends
(#22), whose operators already are the promotion authority; running two
promotion authorities against one replication chain is how split-brain happens.
Users who want Orchestrator should run it themselves — and then they are back
to Option 1, because Orchestrator's promotion flips read_only and ProxySQL
follows it natively.

### Option 3 — Operator-side topology prober

The operator dials seed backends, re-resolves the replication topology
(`SHOW REPLICA STATUS`, `read_only`), and rewrites `mysql_servers` itself. This
re-implements ProxySQL's monitor module — which already runs next to the data
path, per proxy, at 1.5s resolution — but from the control plane, with K8s
reconcile latency, new backend credentials/RBAC, and new failure modes (operator
partition ≠ proxy partition, so the prober can "correct" topology the proxies
see differently). And it still has no promotion authority. Worst of both
options: Orchestrator's complexity surface without Orchestrator's one actual
capability.

### Trade-offs

| | 1. ProxySQL-native | 2. External manager | 3. Operator prober |
| --- | --- | --- | --- |
| Follows writer change | yes, ~1.5s, per proxy | yes, via hooks | yes, reconcile-latency |
| Promotion authority | no (by design) | yes | no |
| New dependency/state | none — works today | heavyweight, stateful | none, but new probe path + creds |
| Failure modes added | none | split-brain vs. backend operators, fencing | operator/proxy partition disagreement |
| Operational cost | document it | deploy, upgrade, babysit | maintain a half-Orchestrator |

## Decision

**Option 1.** The operator's job is to *follow* topology, never to manage it.

1. ProxySQL-native replication-hostgroup failover is the documented, supported
   answer for external backends that have their own HA authority — managed
   cloud databases, Galera, externally run Orchestrator/MHA, platform-managed
   replication: anything that flips `read_only` or moves a writer endpoint.
2. Promotion authority is a declared non-goal of this operator, permanently.
   Backends without any HA authority have an availability problem no proxy
   layer can fix; the answer is "run one", not "the proxy operator becomes one".
3. K8s-native backends get writer-follow via #22 auto-discovery (CR status is
   the topology source there, not read_only polling).

## What changes (follow-ups, all small)

- **Drift read-back becomes replication-hostgroup-aware**: for servers covered
  by a writer/reader pair, compare placement against the *pair* (either
  hostgroup satisfies desired), so the operator stops re-pushing stale
  placement after a monitor-driven move. Ties into the runtime read-back work
  (#16); same code path.
- **`docs/architecture.md` section**: "Failover for external backends" — the
  follow-not-manage stance, the read_only mechanics, the dead-primary
  limitation, monitor-user setup on the backend.
- **e2e scenario**: two in-cluster MySQL pods posing as external endpoints
  (plain addresses in `ProxySQLConfig`, replication hostgroup configured); flip
  `read_only` on them and assert `runtime_mysql_servers` converges the writer
  hostgroup, including across an operator resync (proving the drift fix).
- **Status surfacing (optional, with #16)**: expose the monitor-observed writer
  per replication-hostgroup pair in `ProxySQLConfig.status`, so a writer change
  is visible in `kubectl get pxcfg` without querying the admin port.
- **Candidate later additions**: `mysql_group_replication_hostgroups` /
  `mysql_galera_hostgroups` / Aurora hostgroup tables are the same sync pattern
  (new columns in `sync.go` + `Desired` fields) if demand appears — same
  follow-only semantics, cluster-aware checks instead of read_only.

## Non-goals

- Promoting replicas, fencing, or any write to backend databases beyond the
  existing monitor checks.
- Deploying or managing Orchestrator (or any topology manager) as an operator
  dependency.
- DNS-level writer tracking beyond what ProxySQL already does when a managed
  endpoint repoints (hostname re-resolution).

## Feed-in to #22 (auto-discovery)

The boundary is the topology source, not the backend's location: #22 covers
backends whose topology is published in a watchable CR status; this decision
covers backends whose topology is only observable from the wire. #22 therefore
does not need a probing mode for external backends — its scope stays "watch CRs,
map roles to hostgroups". One interaction to honor in #22's design: when a
backend CR and a replication hostgroup both claim a server set, CR status wins
and the discovery path should not configure read_only-based moves against it —
two followers of the same topology are fine, two writers of `mysql_servers`
rows are not.
