# Running ProxySQL with the operator

An end-to-end walkthrough: install the operator, deploy a ProxySQL cluster
with a read/write split, verify it, and run it day to day. Checked against
release `proxysql-operator-0.8.2`. Each section links to the page that
covers it in depth.

## Overview

The operator runs ProxySQL 3.x on Kubernetes with one Helm chart for the
operator, one CR per ProxySQL cluster, and a separate CR for the
declarative config. The same pods speak MySQL on 6033 and PostgreSQL on
6133, with admin on 6032.

Two CRDs under `proxysql.com/v1alpha1` do all the work:

| Kind | Short name | What it owns |
| --- | --- | --- |
| `ProxySQLCluster` | `pxc` | StatefulSet, headless + ClusterIP Services, a Secret with admin/radmin/monitor passwords (minted by the operator), a Secret with the bootstrap `proxysql.cnf`, a PodDisruptionBudget, an optional ServiceMonitor |
| `ProxySQLConfig` | `pxcfg` | No Kubernetes objects. It runs SQL against each replica's admin port to apply servers, users, rules and variables |

The repo also ships two operator-free charts: `charts/proxysql/`
(stateless data plane, Deployment) and `charts/proxysql-cluster/`
(control-plane node on a persistent disk, StatefulSet). Use those only
where running an operator is off the table. The CRDs are still
`v1alpha1`, so expect field changes between releases.

One naming trap: `pxc` here means ProxySQLCluster, not Percona XtraDB
Cluster. If the Percona operator is installed in the same cluster, use the
full kind name in kubectl.

## Architecture

One Deployment runs both controllers. The cluster controller builds
Kubernetes objects. The config controller owns nothing and talks SQL to
each pod's admin port as `radmin`.

```
 ProxySQLCluster CR ──▶ ┌─ operator Deployment ──────┐
                        │  cluster controller ───────┼──▶ StatefulSet, Services,
 ProxySQLConfig CR ───▶ │  config controller         │    Secrets, PDB
                        └──────────┬─────────────────┘          │
                                   │ SQL as radmin on :6032     │ runs
                    ┌──────────────┼───────────────┐            │
                    ▼              ▼               ▼            ▼
              proxysql-0 ┄┄┄ proxysql-1 ┄┄┄ proxysql-2   (the same pods)
                     ┄┄┄ = ProxySQL Cluster sync between peers
```

ProxySQL's own cluster sync still runs between pods, but the operator
writes to every replica directly. So `Synced` is an honest count, and
restarted pods get config the moment they're Ready. See
[architecture.md](../architecture.md) for the reasoning.

## Prerequisites

You need Kubernetes 1.29 or newer and Helm 3. The chart enforces the
version floor because `spec.networking.tcpKeepalive` renders as
`net.ipv4.tcp_keepalive_*` pod sysctls, which only joined the safe-sysctl
set in 1.29.

- No cert-manager, webhooks or Prometheus Operator required. cert-manager
  only matters if you pick the cert-manager [TLS](tls.md) tier, and a
  missing ServiceMonitor CRD just sets a status condition.
- A reachable backend: any MySQL or PostgreSQL endpoint works.
  [Backends](backends.md) has cookbooks for Oracle MySQL Operator (InnoDB
  Cluster), Percona Server for MySQL (async primary/replica), Percona
  XtraDB Cluster (Galera), mariadb-operator, CloudNativePG and Crunchy PGO.
- A Secret holding each backend user's password, since `ProxySQLConfig`
  references passwords by `passwordSecretRef` rather than inline.
- For local testing, `make kind-up` plus `make operator-image-kind` in the
  repo gives you a kind cluster with the operator image loaded.

## Install the operator

Install once per Kubernetes cluster. The operator is cluster-scoped and
watches both CRDs in every namespace; there is no namespace-restricted
mode.

```bash
helm repo add proxysql https://proxysql.github.io/proxysql-on-k8s
helm repo update
helm install proxysql-operator proxysql/proxysql-operator \
  -n proxysql-system --create-namespace
```

That gives you the two CRDs (`proxysqlclusters.proxysql.com`,
`proxysqlconfigs.proxysql.com`), a single-replica manager Deployment,
cluster-wide RBAC, and a leader-election Role. Check it:

```bash
kubectl -n proxysql-system get deploy
kubectl get crd proxysqlclusters.proxysql.com proxysqlconfigs.proxysql.com
```

The manager image is `ghcr.io/proxysql/proxysql-operator`, and `image.tag`
defaults to the chart's `appVersion`. To mirror it into a private
registry, push that build and leave `tag` empty unless your registry
renames it ([Installation](installation.md#images-and-private-registries)):

```yaml
# values.yaml
image:
  repository: my-registry.example.com/proxysql/proxysql-operator
  tag: ""   # empty = the chart's appVersion; set only if your mirror renames it
imagePullSecrets:
  - name: my-registry-creds
```

For operator HA, scale this one install (`replicaCount: 2`;
`leaderElection.enabled: true` is the default). Don't install the chart
twice in different namespaces: each install gets its own lease, both go
active, and they fight over the same CRs.

## Deploy a ProxySQL cluster

You apply two objects: a `ProxySQLCluster` for the pods and a
`ProxySQLConfig` for what ProxySQL should route. Keep them in the same
namespace, since `clusterRef` doesn't cross namespaces.

First, the backend user's password goes in a Secret:

```bash
kubectl -n app create secret generic app-db-credentials \
  --from-literal=password='S3cret-not-this'
```

Then the cluster. Everything here except `replicas` is optional and shown
so you know where the knobs live:

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: proxysql
  namespace: app
spec:
  replicas: 3
  image:
    repository: proxysql/proxysql
    tag: "3.0"
  resources:
    requests: { cpu: 500m, memory: 512Mi }
    limits:   { memory: 1Gi }
  persistence:
    enabled: true
    size: 1Gi
  protocols:
    mysql: { enabled: true }   # 6033
    pgsql: { enabled: false }  # 6133
  metrics:
    enabled: true              # REST/Prometheus on 6070
    serviceMonitor:
      enabled: true
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
```

And the config. This is a classic primary/replica split: writes to
hostgroup 10, reads to 20, with ProxySQL's monitor moving servers between
them based on `read_only`:

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: proxysql-config
  namespace: app
spec:
  clusterRef:
    name: proxysql
  mysqlReplicationHostgroups:
    - writerHostgroup: 10
      readerHostgroup: 20
      checkType: read_only
  mysqlServers:
    - { hostgroup: 10, hostname: mysql-0.mysql.db.svc, port: 3306 }
    - { hostgroup: 20, hostname: mysql-1.mysql.db.svc, port: 3306, maxReplicationLag: 10 }
    - { hostgroup: 20, hostname: mysql-2.mysql.db.svc, port: 3306, maxReplicationLag: 10 }
  mysqlUsers:
    - username: app
      defaultHostgroup: 10
      passwordSecretRef:
        name: app-db-credentials
        key: password
  mysqlQueryRules:
    - ruleId: 100
      matchDigest: '^SELECT .* FOR UPDATE'
      destinationHostgroup: 10
      apply: true
    - ruleId: 200
      matchDigest: '^SELECT'
      destinationHostgroup: 20
      apply: true
  mysqlVariables:
    mysql-max_connections: "4096"
```

A few things that bite on first use:

- The backend's monitor user needs to exist on MySQL. The operator mints
  the monitor password into the cluster's auth Secret, which is named
  after the cluster (`proxysql` here) unless `spec.auth.secretName` is
  set. Create that user on the backend with the same password, or point
  `spec.auth.secretName` at a Secret you control
  ([the monitor user](backends.md#the-monitor-user)).
- Declared query rules default to `active: 1`, unlike ProxySQL's own
  column default of 0.
- Variable keys use full names with the prefix (`mysql-max_connections`,
  not `max_connections`).
- For PostgreSQL, turn on `protocols.pgsql` and use `pgsqlServers`,
  `pgsqlUsers`, `pgsqlQueryRules`. Declaring pgsql rows against a cluster
  with pgsql off still pushes them but flags `Degraded/PgsqlDisabled`.
- Leave `proxysqlServers` empty. With `replicas > 1` the operator seeds
  the peer list from the headless-service DNS names for ProxySQL Cluster
  sync.

[`examples/`](../../examples/) has ready-made pairs for each supported
backend operator; crib from the one that matches yours.

## Connect and verify

Start with the CR status. `kubectl get pxc` shows Replicas, Ready and
Phase; `kubectl get pxcfg` shows Cluster, Synced, Drifted and Last-Sync.
You want `Phase=Running` and Synced equal to the replica count.

```bash
kubectl -n app get pxc,pxcfg
kubectl -n app describe proxysqlconfig proxysql-config   # conditions + reasons
```

Apps connect to the `proxysql` Service on 6033 (MySQL) or 6133
(PostgreSQL). `status.endpoints` on the cluster lists the in-cluster
host:port for every enabled surface, so you don't have to derive Service
names.

For the admin interface, use radmin. ProxySQL only lets `admin` log in
from localhost, so remote sessions (and the operator itself) go through
`radmin`:

```bash
RADMIN=$(kubectl -n app get secret proxysql -o jsonpath='{.data.radmin-password}' | base64 -d)
kubectl -n app port-forward svc/proxysql 6032:6032 &
mysql -h127.0.0.1 -P6032 -uradmin -p"$RADMIN" --prompt 'admin> '
```

Then check what's actually live at runtime, not just what's in the main
tables:

```sql
SELECT hostgroup_id, hostname, port, status FROM runtime_mysql_servers;
SELECT username, default_hostgroup, active FROM runtime_mysql_users;
SELECT rule_id, match_digest, destination_hostgroup FROM runtime_mysql_query_rules;
SELECT hostname, port, connect_error FROM monitor.mysql_server_connect_log ORDER BY time_start_us DESC LIMIT 5;
SELECT hostgroup, digest_text, count_star FROM stats_mysql_query_digest ORDER BY count_star DESC LIMIT 10;
```

Finally, a real query through the data port:

```bash
kubectl -n app port-forward svc/proxysql 6033:6033 &
mysql -h127.0.0.1 -P6033 -uapp -p -e 'SELECT @@hostname, @@read_only'
```

Run it a few times. Reads should land on the replicas, and
`stats_mysql_query_digest` should show them in hostgroup 20.

## Day-2 operations

Most changes are restart-free. The ones that roll pods are structural
changes to the pod template or the bootstrap cnf.

| Change | What happens |
| --- | --- |
| Edit `ProxySQLConfig` servers, users or rules | SQL push to every ready replica: DELETE + INSERT, then LOAD ... TO RUNTIME + SAVE ... TO DISK. Replication hostgroups and hostgroup attributes share one LOAD/SAVE MYSQL SERVERS. No restart |
| Edit `ProxySQLConfig` variables | `UPDATE global_variables` per key, then one LOAD/SAVE per variable domain. No restart |
| Rotate a user's password Secret | Secret watch triggers a re-push on the next reconcile. No restart |
| Change a value in `ProxySQLCluster.spec.variables` | Pushed at runtime, then read back from `runtime_global_variables`. Rolls pods only if the read-back doesn't match (e.g. `mysql-threads`) |
| Remove a key from `spec.variables`, or add a key the cnf template doesn't already render | Rolling restart via the `proxysql.com/cnf-checksum` annotation |
| Add a key the template already renders (e.g. `mysql-threads`, `admin-cluster_check_*`, eventslog keys) | Only the value changes, so it takes the runtime-push path above |
| `replicas` up or down | StatefulSet scales; new pods get config pushed as soon as they're Ready (pod watch). Peer list re-derived |
| `spec.pause: true` | StatefulSet scaled to 0; Services, Secrets and PVCs kept, `replicas` untouched |
| Image tag, probes, resources, TLS on/off | Normal StatefulSet rolling update |
| TLS cert renewal | `PROXYSQL RELOAD TLS` plus a handshake check per replica. One rolling restart only if leaf and CA swap together |

**Drift.** Every 2 minutes (the `configResyncInterval` chart value) the
operator reads runtime tables back from each replica and re-pushes only to
replicas that drifted. The check is narrow: it compares server membership
and status, user names and default hostgroups, and query-rule IDs
([drift-detection coverage](../reference/admin-tables.md#drift-detection-coverage)).
A hand edit that removes a server, user or rule is reverted. A hand edit
to a variable, a rule body (`match_pattern` and friends), a server weight
or a password is not detected. It is corrected only on the next full push,
which happens when the spec, the pod set or the CR generation changes. If
you want a change to stick, put it in the CR.

Anything the structured fields don't cover goes in `spec.sqlStatements`.
They run verbatim on every replica after the structured config on each
sync pass, with no implicit LOAD/SAVE, so make them idempotent and include
your own. Their effects are not drift-tracked: a hand edit that undoes one
is not reverted until the next full push.

**Scaling and disruptions.** The PDB defaults to
`minAvailable = replicas - 1` and disappears at `replicas: 1`. Spread
replicas across zones with `topologySpreadConstraints`. Keep readiness
probing ProxySQL itself (the default TCP check on 6032); a probe that
queries a backend through the proxy marks every replica NotReady during a
single backend outage.

**Upgrading the operator.** Helm never upgrades CRDs, so apply them
yourself before `helm upgrade`, using the tag that matches the chart (e.g.
`proxysql-operator-0.8.2`):

```bash
helm repo update
kubectl apply -f https://raw.githubusercontent.com/ProxySQL/proxysql-on-k8s/<tag>/charts/proxysql-operator/crds/proxysqlcluster.yaml
kubectl apply -f https://raw.githubusercontent.com/ProxySQL/proxysql-on-k8s/<tag>/charts/proxysql-operator/crds/proxysqlconfig.yaml
helm upgrade proxysql-operator proxysql/proxysql-operator -n proxysql-system
```

Skip the CRD step and new spec fields get stripped at admission, because
the old schema doesn't know them. ProxySQL pods keep serving during an
operator upgrade; they only roll when a release changes the generated pod
template, and release notes call that out (v0.5.0 added `--reload` and
rolled everything once).

**Monitoring.** `spec.metrics.enabled` on the `ProxySQLCluster` (on by
default) exposes ProxySQL's Prometheus endpoint on 6070;
`spec.metrics.serviceMonitor.enabled` creates the ServiceMonitor. Don't
confuse it with the operator chart's own `metrics.enabled`, which is the
operator's metrics endpoint on 8443. For query logs, `spec.logging` adds a
Fluent Bit sidecar that ships the eventslog to stdout, S3 or HTTP
([tutorial 5](../tutorials/05-query-logging.md)).

**Exposing outside the cluster.** Use `spec.service.external` rather than
flipping `service.type`. The external Service carries only data ports by
default; flipping the regular Service to LoadBalancer puts admin on 6032
out there too. `exposeAdmin: true` exists, but pair it with
`loadBalancerSourceRanges` and a NetworkPolicy ([Security](security.md)).

**Config state on disk.** With persistence on, each pod keeps
`proxysql.db` on its PVC and boots with `--reload`. The cnf wins on
restart only for keys the cnf contains: a variable removed from the cnf
keeps its old value from `proxysql.db` (see the eventslog row under
Troubleshooting). The source of truth is still the two CRs, so back those
up (GitOps, Velero) rather than the PVCs.

**Uninstalling.** Delete `ProxySQLConfig` objects first. Each holds the
`proxysql.com/config-cleanup` finalizer that only the operator removes.
Delete one after the operator is gone and it hangs in Terminating.
Reinstall the operator first: it then runs cleanup and releases the
config. Only as a last resort, skip cleanup with
`kubectl annotate --overwrite pxcfg <name> proxysql.com/skip-cleanup=true`
(needs a running operator), or remove just this finalizer by hand. Either
way the config's users, rules and servers stay live on the pods. Remove
only `proxysql.com/config-cleanup`, never the whole `finalizers` list, so
other controllers' finalizers survive:

```bash
kubectl get pxcfg <name> -o json \
  | jq '.metadata.finalizers -= ["proxysql.com/config-cleanup"]' \
  | kubectl replace -f -
```

## Troubleshooting

Start with `kubectl describe` on both CRs; the condition reason usually
names the problem. Then the operator log:
`kubectl -n proxysql-system logs deploy/proxysql-operator`. The full
table is in [Operations](operations.md).

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| pxcfg shows `ClusterFound=False` | `clusterRef.name` wrong, or the cluster is in another namespace | Put both CRs in one namespace; retries every 5s |
| `Ready=False`, reason `UserSecretError` | A `passwordSecretRef` Secret or key is missing | Create it; the Secret watch re-reconciles immediately |
| `Degraded`, reason `AuthSecretError` | `spec.auth.secretName` points at a missing Secret, or a partial schema | Provide all three keys, or username/password |
| Cluster rejects credentials | Password contains `"`, `;` or control characters | Pick passwords without them; they'd break `proxysql.cnf` |
| `spec.variables` key rejected as reserved | Key is owned by auth, protocols, metrics or tls | Set it via that spec field instead |
| Backends stuck SHUNNED: connect failures | Monitor user missing on the backend, or wrong password | Check `monitor.mysql_server_connect_log`; `status.shunnedBackends` counts these |
| Reader shunned while the connect log is clean | Replication lag over `maxReplicationLag` | Check `monitor.mysql_server_replication_lag_log` |
| A server, user or rule removed on the admin port comes back | Drift resync reverting it | Move the change into the CR |
| A hand-edited variable or rule body changes back later | The next full push (spec, pod set or generation change) re-applies the CR | Move the change into the CR; `sqlStatements` if no field covers it |
| New CRD fields ignored after upgrade | CRDs not re-applied | `kubectl apply` the CRDs for the new tag |
| `ProxySQLConfig` stuck Terminating, operator running | Cluster has no ready pods, so cleanup can't run | Bring the pods back and cleanup runs. Last resort: `kubectl annotate --overwrite pxcfg <name> proxysql.com/skip-cleanup=true`, which leaves the config's users, rules and servers live |
| `ProxySQLConfig` stuck Terminating, operator gone | Nothing is left to remove the finalizer | Reinstall the operator; it runs cleanup and releases the config. Last resort: `skip-cleanup` once it's back, or remove only the `proxysql.com/config-cleanup` finalizer (command under Uninstalling) |
| Pods crash-loop after disabling TLS | Dangling cert symlinks on the PVC | The operator adds a `tls-cleanup` init container automatically; check it ran |
| Eventslog keeps running after `queryLog: false` | PVC-backed `proxysql.db` keeps old variables | `UPDATE global_variables ... mysql-eventslog_default_log='false'`, then LOAD/SAVE |
| Pods rejected on older clusters | `tcpKeepalive` sysctls unsafe before 1.29 | Upgrade to 1.29+ or allow them on the kubelet |

One more: removing a variable from `mysqlVariables` stops asserting it but
doesn't restore ProxySQL's default. There's no unset; write the default
value explicitly if you need it back.

## Alternatives

If you'd rather not run this operator, three other routes exist:

| Option | When it fits |
| --- | --- |
| `charts/proxysql` / `charts/proxysql-cluster` from this repo | No operator allowed; you manage config yourself |
| Percona Operator for PXC | ProxySQL bundled as the PXC load balancer (`proxysql.size`, custom `proxysql.cnf` via ConfigMap). Galera only |
| KubeDB ProxySQL | ProxySQL CR with backend + `syncUsers`, version updates via `ProxySQLOpsRequest`. Open-core licensing |

## See also

- [Installation](installation.md), [Configuration](configuration.md),
  [Operations](operations.md), [Backends](backends.md)
- [ProxySQLCluster reference](../reference/proxysqlcluster.md),
  [ProxySQLConfig reference](../reference/proxysqlconfig.md)
- [Status & conditions](../reference/status.md),
  [Annotations & finalizers](../reference/annotations.md)
