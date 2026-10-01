# Galera hostgroups: `ProxySQLConfig.spec.mysqlGaleraHostgroups`

**Date:** 2026-09-29
**Status:** Approved design, implemented
**Decisions:** follow-only semantics (no promotion, no probing) · field on
`ProxySQLConfig` (no new CRD) · shares the `MYSQL SERVERS` LOAD with
`mysql_servers` · all four hostgroups of a row form ONE drift equivalence
class · Galera only (group replication and Aurora out of scope)

## Background

[`2026-06-10-external-failover-design.md`](2026-06-10-external-failover-design.md)
listed `mysql_galera_hostgroups` under "Candidate later additions" — the same
sync pattern as `mysql_replication_hostgroups`, with "same follow-only
semantics, cluster-aware checks instead of read_only". This implements that.

Galera users can already push the table through `spec.sqlStatements`, but that
workaround is actively harmful: `Desired.Drift` does not know the Galera
hostgroups, so every runtime read-back sees the Galera monitor's legitimate
moves (a node parked in the backup-writer or offline hostgroup) as drift. The
operator then re-pushes the spec's static placement, briefly misrouting
traffic until the monitor corrects it again — the same failure mode
`mysql_replication_hostgroups` had in #34, one topology later.

## API

New optional field on `ProxySQLConfigSpec`, right after
`MySQLReplicationHostgroups`:

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
spec:
  clusterRef: {name: proxysql}
  mysqlServers:
    - {hostgroup: 10, hostname: pxc-0.pxc, port: 3306}
    - {hostgroup: 10, hostname: pxc-1.pxc, port: 3306}
    - {hostgroup: 10, hostname: pxc-2.pxc, port: 3306}
  mysqlGaleraHostgroups:
    - writerHostgroup: 10
      backupWriterHostgroup: 12
      readerHostgroup: 11
      offlineHostgroup: 13
      maxWriters: 1
      writerIsAlsoReader: 2
```

`MySQLGaleraHostgroup` maps 1:1 to the nine columns of ProxySQL's
`mysql_galera_hostgroups` table:

| field | Go type | validation | renders when unset |
| --- | --- | --- | --- |
| `writerHostgroup` | `int32` | required, min 0 | — |
| `backupWriterHostgroup` | `int32` | required, min 0 | — |
| `readerHostgroup` | `int32` | required, **min 1** | — |
| `offlineHostgroup` | `int32` | required, min 0 | — |
| `active` | `*bool` | optional | `1` |
| `maxWriters` | `*int32` | optional, min 0 | `1` |
| `writerIsAlsoReader` | `*int32` | optional, enum 0;1;2 | `0` |
| `maxTransactionsBehind` | `*int32` | optional, min 0 | `0` |
| `comment` | `string` | optional | `''` |

`active` is `*bool` per the repo's default-true-boolean convention: a plain
`bool` would marshal `active: false` away and re-default to true.

- `+listType=map`, `+listMapKey=writerHostgroup` (ProxySQL's PRIMARY KEY),
  `+kubebuilder:validation:MaxItems=64` to keep list-level CEL inside the
  cost budget.
- **Item-level CEL** mirrors ProxySQL's CHECK constraints: the four
  hostgroups must be pairwise distinct.
- **List-level CEL** mirrors ProxySQL's three UNIQUE constraints:
  `readerHostgroup`, `offlineHostgroup` and `backupWriterHostgroup` are each
  unique across rows. Admission rejects violations that would otherwise only
  surface as a sync-time SQL error on every replica.

## Sync placement

New `mysql_galera_hostgroups` step in `Sync`, after
`mysql_replication_hostgroups` and before `mysql_servers_apply`:
`DELETE FROM mysql_galera_hostgroups` unconditionally, then one multi-row
INSERT of all nine columns. Unset NOT NULL columns render ProxySQL's column
default via `defInt32`/`defBoolAsInt`, never NULL.

No new LOAD/SAVE section: the table is part of `MySQL_HostGroups_Manager`'s
servers commit, so `LOAD MYSQL SERVERS TO RUNTIME` (already issued by
`mysql_servers_apply`) activates it, and `SAVE MYSQL SERVERS TO DISK`
persists it. It is also covered by the ProxySQL Cluster `mysql_servers_v2`
checksum, so cluster sync propagates it to peers unchanged.

The table becomes operator-owned, exactly like every other synced section.

## Drift semantics

The Galera monitor moves nodes among **writer, backup-writer, reader and
offline** hostgroups, and with `writer_is_also_reader` 1 or 2 a node sits in
two of them at once. All four hostgroups of a Galera row therefore compare as
one equivalence class — the four-member generalization of the replication
pair's two-member class.

`replicationClasses` is generalized to take *groups* of hostgroups rather than
pairs: `{writer, reader}` from each replication pair plus
`{writer, backupWriter, reader, offline}` from each Galera row, with every
member of a group unioned together. The "smallest id is the class
representative" rule and the `rhg<rep>:` canonical-key prefix are unchanged,
so existing drift messages and tests are untouched. Groups sharing a
hostgroup still chain into one class via union-find.

Including the offline hostgroup is deliberate: a node the monitor parked
there is still a declared member of the topology, not drift. A node absent
from all four hostgroups is real drift; an unknown node present in any of
them is "extra". Hostgroups outside every row keep exact placement, and
`pgsql_servers` remain exact — this operator carries no PostgreSQL
cluster-hostgroup concept.

Like `mysql_replication_hostgroups`, the `mysql_galera_hostgroups` table
itself is not drift-tracked: it is loaded and saved together with
`mysql_servers`, so the realistic external mutation (a wipe of the servers
table) is already caught by the server-membership comparison, and every drift
push re-asserts all tables anyway.

## Non-goals

- **No promotion, no fencing, no probing.** The operator syncs the table and
  tolerates the monitor's moves. ProxySQL's own Galera check
  (`wsrep_local_state`, `wsrep_desync`, `wsrep_reject_queries`,
  `read_only`) does the observing; the operator never writes to a backend.
- **No group replication.** `mysql_group_replication_hostgroups` has
  identical columns and identical four-hostgroup drift semantics, so it is a
  mechanical follow-up reusing this row struct, INSERT helper and drift
  grouping — but it is not in this change.
- **No Aurora.** `mysql_aws_aurora_hostgroups` has a different column set and
  is AWS-only.
- **No status surfacing.** Reporting the monitor-observed writer per Galera
  row in `ProxySQLConfig.status` stays the optional idea it is in the
  external-failover design.

## Upgrade notes

- **No pod restart.** The bootstrap cnf and the StatefulSet pod template are
  untouched; `builders/golden_test.go` passes without regenerating goldens.
- **One-time config re-push per `ProxySQLConfig`.** `syncFingerprint` is a
  SHA over `json.Marshal(Desired)`, and the new `Desired` field changes the
  hash even when the spec leaves it empty. Harmless, and the same one-time
  re-push every earlier field addition caused.
- **The operator now owns `mysql_galera_hostgroups`.** Rows inserted by hand
  on the admin port are deleted on the next sync unless declared in the
  spec. Rows pushed via `sqlStatements` keep working — those statements run
  after the structured sections, so they re-insert on every pass — but the
  structured field is now the supported path.

## Testing

- `sync_test.go`: the unconditional DELETE appears in the empty-desired pass;
  a full row and a defaults-only row render the right column order and the
  `1,1,0,0` defaults; the INSERT precedes `LOAD MYSQL SERVERS TO RUNTIME`.
- `runtime_test.go`: a 3-node Galera fixture (row 10/12/11/13) pins that a
  writer plus two backup-writers, `writer_is_also_reader=2` mirroring into
  the reader hostgroup, and a node parked in offline are all *not* drift;
  that a node absent from all four is drift naming its spec placement; that
  an unknown node is "extra"; that a Galera row and a replication pair
  sharing a hostgroup chain into one class; and that hostgroups outside any
  row stay exact.
- `union_test.go`, `cleanup_desired_test.go`, `fingerprint_test.go`: merge by
  writer hostgroup with last-writer-wins, cleanup clears the section, a new
  row changes the fingerprint.
- envtest: CRD admission rejects equal hostgroups, `readerHostgroup: 0`,
  `writerIsAlsoReader: 3`, and duplicate reader hostgroups across rows.
- e2e (`test/e2e/scenarios/galera.sh`): two Galera-enabled MariaDB backends
  declared in the writer hostgroup; assert one ONLINE writer and one
  backup-writer, that flipping `read_only` on the writer moves it and
  promotes the peer, and — the point of the drift change — that an operator
  resync does not revert the monitor's placement and `driftedReplicas` stays
  0.

## Acceptance

- `make manifests && make sync-crds` clean, both CRD copies identical.
- `Executor` interface untouched; builders untouched; `TestGolden` green
  without `UPDATE_GOLDEN`.
- All existing tests pass unchanged.
