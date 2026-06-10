# Backend Auto-Discovery: Design

**Date:** 2026-06-10
**Status:** Approved design, pending implementation plan
**Issue:** #22 (roadmap §3.1)
**Decision:** a `backendSources` list on `ProxySQLConfig` (no dedicated CR in
v1) · initial kinds CloudNativePG and MariaDB Operator · prefer the backends'
stable role Services over per-pod discovery · topology source is CR status,
never wire probing (per the external-failover decision)

## Context

Today backends are listed by hand in `ProxySQLConfig.spec.mysqlServers` /
`pgsqlServers`. The two live nightly examples already show what users must
hand-maintain: the CNPG example points at the `-rw`/`-ro` Services, the
MariaDB example lists three pod FQDNs plus a replication hostgroup so
ProxySQL's monitor follows `read_only` flips. Auto-discovery replaces that
hand-maintenance with a watch on the backend operator's CR.

Scope boundary, fixed by the external-failover decision
(`2026-06-10-external-failover-design.md`): **#22 covers backends whose
topology is published in a watchable CR status.** Backends observable only
from the wire stay on ProxySQL-native `mysql_replication_hostgroups` +
`read_only` monitoring. No probing or seeded-discovery mode exists here.

## API shape: `backendSources` on ProxySQLConfig

```yaml
spec:
  clusterRef: {name: proxysql}
  backendSources:
    - name: pg                     # entry name, used in status
      kind: cnpg                   # enum: cnpg | mariadb (v1)
      clusterName: pg              # the backend CR's name; XOR labelSelector
      # labelSelector: {matchLabels: {team: payments}}
      hostgroups:
        writer: 0
        reader: 1
      endpointMode: service        # service (default) | pod
      port: 5432                   # optional override; default per kind
      serverDefaults:              # optional, applied to every emitted row
        weight: 1
        maxConnections: 200
        useSSL: false
```

A list stanza on `ProxySQLConfig` beats a dedicated CR for v1:

- **Config-adjacent.** Discovered servers are inputs to the same `Desired`
  the document already produces; hostgroup numbers, users, and query rules
  referencing them live in the same object. Splitting the source from the
  config that consumes it invites dangling references.
- **Reuses everything.** The existing build→hash→write-to-all pipeline,
  drift resync, Secret/Pod/Cluster watches, finalizer cleanup, and status
  conditions all apply unchanged. A dedicated CR would need its own
  reconciler, status, finalizer, and RBAC story.
- **No new user-facing RBAC surface.** Whoever can edit `ProxySQLConfig` can
  already define servers; discovery adds no privilege.

Escape hatch: M3.2's granular-CR design establishes per-cluster aggregation
of multiple sources. If multi-tenancy later demands a separately-RBAC'd
`ProxySQLBackendSource` CR, it slots into that aggregator as one more source
kind — the stanza's entry schema is written to be liftable verbatim.

## Per-kind mapping (v1 kinds)

| | cnpg (`postgresql.cnpg.io/v1 Cluster`) | mariadb (`k8s.mariadb.com/v1alpha1 MariaDB`) |
| --- | --- | --- |
| service mode, writer | `<name>-rw` Service, port 5432 | `<name>-primary` Service, port 3306 |
| service mode, reader | `<name>-ro` Service (standbys only) | `<name>-secondary` Service |
| pod mode, writer | `status.currentPrimary` → pod FQDN | `status.currentPrimary` → pod FQDN via the internal headless Service |
| pod mode, readers | `status.instanceNames` minus primary | replica ordinals minus primary |
| target table | `pgsql_servers` | `mysql_servers` |

**Prefer `endpointMode: service`** (the default), and the design leans on
this insight: the backend operators already publish *stable role endpoints*
— CNPG re-points `-rw` during failover, mariadb-operator re-points
`-primary`. In service mode a failover requires **no ProxySQL update at
all**: the rows in `mysql_servers`/`pgsql_servers` never change, the Service
endpoint underneath them does. Zero hostgroup churn, zero sync latency,
nothing to converge. Consequence: hostgroup churn on failover only exists in
`pod` mode, which is only needed when users want per-replica routing or
weights (e.g. weighting readers differently, draining one replica). The docs
and examples will say exactly that: use `service` unless you need per-pod
control.

In `pod` mode the operator is the follower of `status.currentPrimary`: a
primary change flips row placement between writer/reader hostgroups, changes
the `Desired` hash, and triggers a write-to-all push. ProxySQL's monitor is
**not** configured against discovered hostgroups (see conflicts below).

## Watch mechanics

- **Per-kind GVK registry**: a small table mapping `kind` enum →
  GroupVersionKind + role-resolution function. Adding Percona/Crunchy/
  InnoDBCluster later is one registry entry plus RBAC.
- **Dynamic, unstructured watches** — the ServiceMonitor pattern from
  `proxysqlcluster_controller.go`: the operator never imports backend
  operator Go APIs. Watches are registered lazily per kind the first time a
  `ProxySQLConfig` references it, via the manager cache with
  `unstructured.Unstructured` sources; the map handler enqueues every config
  whose `backendSources` select the changed CR (name or labelSelector
  match, same namespace only). A predicate filters to generation/status
  changes so reader-pod churn doesn't thrash reconciles.
- **Absent CRDs degrade gracefully.** Third-party CRDs may not be installed.
  RBAC for each kind is gated by Helm values
  (`discovery.cloudnativepg.enabled`, `discovery.mariadb.enabled`, default
  off) so the ClusterRole only asks for `get;list;watch` on CRs that exist.
  At reconcile time a RESTMapper `NoMatch` (or watch-registration failure)
  marks that source `CRDMissing` and the reconcile **continues** with the
  remaining sources and static lists — one missing backend operator must not
  block the whole config.

## Merge and conflict semantics

Discovered rows merge with the static `mysqlServers`/`pgsqlServers` lists
into the **same `Desired`** → same fingerprint → same write-to-all. No
second sync path; drift resync and the informed re-push see one unified row
set.

- **Static vs discovered collision** (same hostname:port emitted by both): a
  `Degraded` condition (`reason: StaticOverlap`) names the rows; the
  discovered row wins, because in pod mode it carries live role placement
  and a stale static row would re-introduce a demoted writer. The fix is
  always "delete the static row".
- **Replication hostgroups vs discovery**: per the external-failover
  decision, two *followers* of one topology are fine but two *writers* of
  placement are not. A `mysqlReplicationHostgroups` pair whose
  writer/reader hostgroups overlap a source's hostgroups is rejected to
  `Degraded` (`reason: TopologyAuthorityConflict`) — CR status is the
  authority for discovered hostgroups; `read_only`-driven moves must not
  fight it.
- **Interaction with #34 pair-aware drift**: pair-aware relaxation applies
  only to declared replication-hostgroup pairs. Discovered hostgroups never
  have one (enforced above), so drift for them stays exact-placement —
  correct, since the operator itself owns placement there.

## Status surfacing

`status.backendSources[]`, one entry per stanza entry: `name`, `kind`,
`discoveredServers` (count), `currentWriter` (hostname, pod mode only),
`lastObservedGeneration` of the backend CR, and a per-source condition-style
`reason`/`message`: `Discovered`, `CRDMissing`, `BackendNotFound`,
`SelectorMatchesNothing`, `StatusIncomplete` (CR exists but no
`currentPrimary` yet — rows withheld until topology is published). The
config-level `Ready`/`Degraded` conditions aggregate these.

## Testing

- **Unit**: per-kind role-resolution functions are pure (unstructured in,
  rows out) — table tests with captured real CNPG/MariaDB status payloads.
- **envtest**: install minimal fake CNPG/MariaDB CRDs from testdata; create
  unstructured backend CRs, assert merged `Desired`, flip
  `status.currentPrimary`, assert hash change and row re-placement; delete
  the CRD-less kind's source, assert `CRDMissing` plus continued sync of the
  rest.
- **e2e (nightly)**: extend the live `examples/postgresql/cloudnativepg` and
  `examples/mysql/mariadb-operator` flavors with `backendSources` variants.
  Kill-the-primary assertion: delete the primary pod, assert the writer
  hostgroup converges in `runtime_*_servers` (pod mode) and that traffic
  through 6033/6133 recovers with no row change (service mode).

## Non-goals (v1)

- Probing or seeded discovery of any kind — settled by the external-failover
  decision; wire-observable backends use replication hostgroups.
- Percona PS/PXC, Oracle InnoDBCluster, Crunchy PGO kinds — follow once the
  registry exists; each is an entry + RBAC + nightly example.
- Cross-namespace discovery — sources resolve in the config's namespace only.
- A dedicated backend-source CR — revisit with M3.2 multi-tenancy.
- Discovering users/credentials from backend CRs — servers only.
