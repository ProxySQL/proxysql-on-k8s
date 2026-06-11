# Managing ProxySQL clusters

Day-to-day operations on `ProxySQLCluster` resources: sizing, credentials,
storage, protocols, exposure, scheduling, and what actually happens when
you scale or change things. Written for the DBA or platform engineer who
owns running clusters. For the exhaustive field list see the
[ProxySQLCluster reference](../reference/proxysqlcluster.md); for the SQL
configuration pushed *into* the cluster see
[Configuration](./configuration.md).

A `ProxySQLCluster` reconciles into: a StatefulSet, a regular ClusterIP
Service plus a headless Service, an auth Secret, a bootstrap-cnf Secret
(`<cluster>-cnf`), an optional PodDisruptionBudget, and an optional
ServiceMonitor.

## Sizing and replicas

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: proxysql
spec:
  replicas: 3            # default 3, minimum 1
  resources:
    requests: {cpu: 500m, memory: 512Mi}
    limits: {cpu: "2", memory: 1Gi}
```

ProxySQL replicas are independent proxies behind one Service — scale for
throughput and for surviving node loss. Each pod runs ProxySQL with 4
worker threads (set in the bootstrap cnf); size CPU requests accordingly.

Note: scaling a multi-replica cluster **rolls all pods**, not just the
new/removed ones. The bootstrap cnf embeds the per-pod peer list for
ProxySQL Cluster sync, so changing `replicas` changes the cnf, and the
cnf checksum triggers a rolling restart (see
[Rolling updates](#rolling-updates)). Plan scale operations like you
would plan a restart.

## Auth secrets

Every cluster needs three credentials: `admin` (localhost-only inside
the pod), `radmin` (the remote-capable admin account the operator uses
to push config), and `monitor` (what ProxySQL uses to health-check
backends). They live in one Secret.

**Operator-minted (default).** Leave `spec.auth` empty and the operator
creates a Secret named after the cluster with random 32-character
passwords under keys `admin-password`, `radmin-password`,
`monitor-password`. Existing values are preserved across reconciles;
missing keys are backfilled.

**Bring your own.** Set `spec.auth.secretName` to an existing Secret.
Two data schemas are accepted:

```yaml
# Schema 1 — operator schema (all three keys required; names overridable
# via spec.auth.keys):
apiVersion: v1
kind: Secret
metadata: {name: my-proxysql-auth}
stringData:
  admin-password: "..."
  radmin-password: "..."
  monitor-password: "..."
---
# Schema 2 — platform schema (what most platform tooling emits).
# admin and radmin share the password; monitor-password is an optional
# extra key. A username other than admin/radmin becomes an additional
# remote-capable admin login.
apiVersion: v1
kind: Secret
metadata: {name: my-proxysql-auth}
stringData:
  username: platform
  password: "plat-secret"
```

A *partial* operator schema (e.g. `admin-password` present without the
other two) is rejected outright rather than silently falling through to
schema 2. Passwords must not contain `"`, `;`, or control characters,
and usernames must match `[A-Za-z0-9_.-]+` — these values are rendered
into the cnf grammar and could never work otherwise. If the referenced
Secret is missing or invalid the cluster goes `Degraded` with reason
`AuthSecretError`. Details in [Security](./security.md).

**Rotation behavior.** Changing the auth Secret re-renders the
`<cluster>-cnf` Secret, and the cnf checksum rolls the pods. On a
cluster with **persistence disabled** that is the whole story: pods come
back with the new credentials. With **persistence enabled**, read the
next section carefully.

## Persistence trade-offs

```yaml
spec:
  persistence:
    enabled: true        # default true; set false explicitly to disable
    size: 1Gi            # default
    storageClass: fast   # optional
```

With persistence on, each pod gets a PVC mounted at `/var/lib/proxysql`
holding `proxysql.db` — ProxySQL's own SQLite config store. With it off,
an emptyDir is used and every restart starts blank (the operator's
pod-watch re-pushes `ProxySQLConfig` within seconds, so this is less
scary than it sounds).

**The precedence rule that matters:** the bootstrap `proxysql.cnf` is
only authoritative on the *first* start against an empty data
directory. After that, **`proxysql.db` wins over the cnf** on every
restart. Consequences:

- Settings carried by the bootstrap cnf — admin credentials, listener
  interfaces, monitor credentials, query-log (eventslog) variables —
  do not change on a persistent cluster just because the cnf changed
  and the pods rolled. ProxySQL reloads its previous state from disk.
- Rotating the auth Secret on a persistent cluster can therefore leave
  pods running the *old* admin password while the operator tries the
  new one, surfacing as sync failures (`PartialSync`). Prefer changing
  such values through the admin interface / `ProxySQLConfig` variables
  (which are pushed via SQL and `SAVE ... TO DISK`, so they survive
  restarts correctly), or recreate the PVCs to re-bootstrap.
- Toggling `spec.logging.queryLog` off does not stop an already-running
  eventslog on a persistent cluster, for the same reason — see
  [Tutorial 05 — query logging](../tutorials/05-query-logging.md).

PVCs are retained when the cluster (or its StatefulSet) is deleted —
delete them explicitly if you want a clean slate.

## Protocols and ports

```yaml
spec:
  protocols:
    mysql: {enabled: true, port: 6033}    # default on, 6033
    pgsql: {enabled: true}                # default off; port 6133 when on
    admin: {port: 6032}                   # always on, cannot be disabled
    web:   {port: 6080}                   # default off; HTTPS stats UI
```

Rules, as implemented:

- A **non-zero port implies enabled** for protocols left without an
  explicit `enabled` (`pgsql: {port: 6133}` turns pgsql on).
- An explicit `enabled` always wins over the port heuristic — except
  **admin**, which is always on regardless of `enabled: false`: the
  operator needs it to push configuration.
- `mysql` defaults to on; `pgsql` and `web` default to off.
- The metrics endpoint (`spec.metrics`, default on, port 6070) is
  ProxySQL's REST/Prometheus exporter — see
  [Tutorial 06 — monitoring](../tutorials/06-monitoring.md).

`status.endpoints` publishes the in-cluster `host:port` for every
enabled surface, so consumers never have to re-derive defaults.

## Exposing the Service

The operator creates two Services: `<cluster>` (regular ClusterIP, what
applications connect to) and `<cluster>-headless` (StatefulSet pod DNS;
leave it alone). `spec.service` customizes the regular one only:

```yaml
spec:
  service:
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-internal: "true"
    sessionAffinityTimeoutSeconds: 300   # enables ClientIP affinity
```

**Annotation merge semantics:** annotations are *merged*, not owned
wholesale. Keys from the spec win; annotations written by other
controllers (cloud LB controllers stamp Services constantly) are
preserved. The flip side: a key you *remove* from
`spec.service.annotations` lingers on the Service until you remove it by
hand — the operator cannot tell a removed spec key from a foreign one.

**External exposure:** the operator-managed Service is always
`ClusterIP`. To expose ProxySQL outside the cluster, create your own
`LoadBalancer` Service or use your ingress/gateway of choice, selecting
the pods with the stable selector labels:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: proxysql-external
spec:
  type: LoadBalancer
  selector:
    app.kubernetes.io/name: proxysql
    app.kubernetes.io/instance: proxysql   # = cluster name
    proxysql.com/cluster: proxysql
  ports:
    - {name: mysql, port: 3306, targetPort: mysql}
```

Think before exposing the admin port (6032) beyond the cluster — see
[Security](./security.md#network-exposure-surface).

## TCP keepalive

Long-lived idle client connections through cloud NAT/LB layers die
silently; keepalives keep them honest:

```yaml
spec:
  networking:
    tcpKeepalive: {time: 120, interval: 30, probes: 5}
```

These render as pod sysctls (`net.ipv4.tcp_keepalive_{time,intvl,probes}`),
admitted under PSA `restricted` on Kubernetes ≥ 1.29. On older clusters
the kubelet rejects the pod (`SysctlForbidden`) unless the sysctls are
explicitly allowed — see [Operations](./operations.md#troubleshooting).
Unset fields keep the node's kernel defaults.

## Scheduling

`spec.nodeSelector`, `spec.tolerations`, and `spec.affinity` pass
through to the pod template. No affinity is applied by default — if you
want replicas spread across nodes (you usually do for `replicas > 1`),
set pod anti-affinity explicitly:

```yaml
spec:
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            topologyKey: kubernetes.io/hostname
            labelSelector:
              matchLabels: {proxysql.com/cluster: proxysql}
```

## Security posture (PSA `restricted`)

Pods run `restricted`-compatible by default: `runAsNonRoot`, uid/gid
999, `readOnlyRootFilesystem`, all capabilities dropped, RuntimeDefault
seccomp — including the optional Fluent Bit sidecar. The
`podSecurityContext` / `containerSecurityContext` fields exist for image
quirks, but the project's stance is: if a change requires loosening the
restricted posture, find another way. Everything the operator produces
is admitted in a namespace enforcing
`pod-security.kubernetes.io/enforce: restricted` as-is.

## Scaling, PDB, and ProxySQL Cluster sync

When `replicas > 1`, two things switch on:

- **PodDisruptionBudget** (default on, omitted at `replicas ≤ 1`):
  `minAvailable = replicas - 1` (so 1 for a 2-replica cluster). Override
  with `spec.podDisruptionBudget.minAvailable` / `maxUnavailable`, or
  disable with `enabled: false`.
- **ProxySQL Cluster sync**: the bootstrap cnf lists every peer pod's
  stable DNS in `proxysql_servers` and enables the `cluster_*` admin
  variables (sync runs as `radmin`). This is belt-and-braces — the
  operator still pushes `ProxySQLConfig` to **every** replica directly,
  and `status.syncedReplicas` tracks those direct writes, not cluster
  sync. See [Configuration](./configuration.md#the-write-to-all-model).
  A `ProxySQLConfig` does not disturb this: when its `proxysqlServers`
  list is empty, each config sync auto-populates the peer table from the
  same per-pod DNS names
  ([reference](../reference/proxysqlconfig.md#proxysqlservers)).

## Rolling updates

The pod template carries a `proxysql.com/cnf-checksum` annotation — a
hash over the rendered bootstrap cnf (and the Fluent Bit config when
logging is enabled). Any change that alters the cnf rolls the pods:
auth Secret contents, protocol/port changes, metrics or web toggles,
replica count (peer list), logging settings. Image and resource changes
roll the pods the normal StatefulSet way. Pod management is `Parallel`,
so initial creation doesn't serialize, while updates follow the
StatefulSet rolling-update semantics.

Watch a rollout:

```bash
kubectl get pxc proxysql            # PHASE column: Running / Updating
kubectl rollout status sts/proxysql
```

## Next

- [Configuration](./configuration.md) — backends, users, query rules.
- [Operations](./operations.md) — status fields, troubleshooting, logs.
- [Tutorial 04 — high availability](../tutorials/04-high-availability.md).
