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

**Rotation behavior.** Rotating `admin`/`radmin` re-renders the
`<cluster>-cnf` Secret (the `admin_credentials` cnf line changes) and the
cnf checksum rolls the pods. Rotating `monitor` is different: its
password is an ordinary variable value, not part of `admin_credentials`,
so the operator applies it at runtime (`UPDATE` + `LOAD ... TO RUNTIME` +
`SAVE ... TO DISK`) with **no restart** — see [configuration changes:
runtime vs
restart](../reference/proxysqlcluster.md#configuration-changes-runtime-vs-restart).
On a cluster with **persistence disabled**, an admin/radmin rotation is
the whole story: pods come back with the new credentials. With
**persistence enabled**, read the next section carefully.

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

**The precedence rule that matters:** the container runs
`proxysql --reload`, so on **every** start ProxySQL merges the bootstrap
`proxysql.cnf` over the persisted `proxysql.db`: for a setting present in
**both**, the cnf value wins; a setting present **only in the db** (set at
runtime and `SAVE`d, or removed from the cnf since) is left untouched; the
merged result is saved back to `proxysql.db`. (Upstream documents the
`--reload` merge as best-effort — "no guarantee … validate that the merge
was as expected" — so verify on the admin port after anything
security-sensitive.) Consequences:

- Settings carried by the bootstrap cnf — admin credentials, listener
  interfaces, `spec.variables`, query-log (eventslog) variables — **do**
  take effect on a persistent cluster when the cnf changes and the pods
  roll: the restarted pod re-applies the cnf lines over its `proxysql.db`.
  Monitor credentials additionally land restart-free (pushed at runtime
  with `SAVE ... TO DISK`).
- Rotating `admin`/`radmin` on a persistent cluster therefore works: the
  rotation rolls the pods and each one comes back with the new
  `admin_credentials` line merged from the cnf. Given the upstream
  best-effort caveat above, verify after rotating (a failed merge would
  surface as `PartialSync` while the operator dials with the new
  password).
- What does **not** converge on restart is *removal*: a line deleted from
  the cnf keeps its old value in `proxysql.db` — the merge never deletes
  db entries. Toggling `spec.logging.queryLog` **off** is the canonical
  example: it removes the `eventslog_*` cnf lines, so an already-running
  eventslog keeps running — see
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

The operator creates two Services by default: `<cluster>` (the regular
Service, what in-cluster applications connect to) and `<cluster>-headless`
(StatefulSet pod DNS; leave it alone). `spec.service` customizes the regular
one and, optionally, adds a curated third Service for out-of-cluster
clients. There are two paths, and they're independent — pick one, or
combine them:

- **Flip `spec.service.type`** — the simple path. Changes the type of the
  *existing* regular Service in place; no new object, the ClusterIP is
  retained.
- **Turn on `spec.service.external`** — the curated path. Creates a
  *second, independent* Service, `<cluster>-external`, that carries only
  the ports you choose. One Kubernetes Service carries multiple ports, so
  exposing mysql, pgsql, and metrics externally still only needs this one
  object — there's no per-port LoadBalancer to provision.

### Path 1: flip the regular Service's type

```yaml
spec:
  service:
    type: LoadBalancer   # ClusterIP (default) | NodePort | LoadBalancer
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-internal: "true"
    sessionAffinityTimeoutSeconds: 300   # enables ClientIP affinity
```

**The footgun:** every enabled port rides this Service — mysql, pgsql,
web, metrics, and **admin (6032)**, always. There is no way to flip
`service.type` to `LoadBalancer`/`NodePort` without also putting the
admin interface on the same edge. If you want the data plane exposed but
admin kept in-cluster, use Path 2 instead — its default port set excludes
admin. Tuning fields like `loadBalancerSourceRanges` or
`externalTrafficPolicy` are not available on this path (there's no
per-Service tuning block for the regular Service, only annotations); if
you need them, use the curated external Service.

**Annotation merge semantics:** annotations are *merged*, not owned
wholesale. Keys from the spec win; annotations written by other
controllers (cloud LB controllers stamp Services constantly) are
preserved. The flip side: a key you *remove* from
`spec.service.annotations` lingers on the Service until you remove it by
hand — the operator cannot tell a removed spec key from a foreign one.

### Path 2: a curated external Service

```yaml
spec:
  service:
    external:
      enabled: true
      type: LoadBalancer            # default; NodePort is the other option
      loadBalancerSourceRanges: ["203.0.113.0/24"]
      externalTrafficPolicy: Local
      # ports: {}                   # omitted = default set (see below)
      # exposeAdmin: false          # default; see the warning below
```

`<cluster>-external` is independent of the regular Service: its own type,
its own annotations (not merged with `spec.service.annotations`), its own
tuning surface (`loadBalancerClass`, `externalTrafficPolicy`,
`internalTrafficPolicy`, `loadBalancerSourceRanges`,
`allocateLoadBalancerNodePorts`, `healthCheckNodePort`, `ipFamilyPolicy`,
`ipFamilies`). Full field list in the [ProxySQLCluster
reference](../reference/proxysqlcluster.md#external-service).

**Default port policy:** an empty (or omitted) `ports` map yields
**data-plane traffic only** — `mysql` + `pgsql`, and only for whichever of
those protocols is enabled on the cluster. `web` and `metrics` are never
in the default set; list them explicitly under `ports` (and have the
matching `spec.protocols`/`spec.metrics` toggle on) to add them:

```yaml
spec:
  service:
    external:
      enabled: true
      ports:
        mysql: {}
        metrics: {nodePort: 30070}   # pin a node port; 0/omitted = auto
```

**The `exposeAdmin` warning.** Setting `service.external.exposeAdmin: true`
puts ProxySQL's admin interface — full control over routing, users, and
backends — on a network edge. It is deliberately its own boolean, not a
`ports` entry (`admin` is not even a valid `ports` key), so a reviewer can
find every externally admin-exposed cluster by grepping this one field.
Pair it with `loadBalancerSourceRanges` and a NetworkPolicy; see
[Security](./security.md#network-exposure-surface) for the full
recommendation before turning this on.

**LB-pending semantics.** `status.endpoints.external` mirrors the *live*
external Service, not the spec, because LoadBalancer provisioning is
asynchronous:

- **LoadBalancer:** `"host:port"` once the cloud provider assigns an
  ingress IP or hostname — **empty until then**. Don't assume it's
  populated the reconcile after `enabled: true`; poll
  `kubectl get pxc <cluster> -o jsonpath='{.status.endpoints.external}'`
  or `kubectl get svc <cluster>-external`.
- **NodePort:** the comma-separated allocated node ports, in port order,
  as soon as the apiserver allocates them (no host — every node's IP
  serves them).

**The `ipFamilies` immutability caveat.** Kubernetes rejects mutating
`ipFamilies` on an already-created Service. If you change
`service.external.ipFamilies` on a cluster whose external Service already
exists, the apply fails, and the cluster goes `Degraded`/reason
`ExternalServiceError` (the rest of the reconcile — StatefulSet, PDB,
ServiceMonitor — still applies; nothing else is wedged). To actually
change families, toggle `service.external.enabled` off then back on (or
remove and re-add the block) so the Service is deleted and recreated
rather than mutated in place.

**Disabling** (`enabled: false`, or removing the `external` block) deletes
the `<cluster>-external` Service. It's also retained across
`spec.pause: true` — pausing only scales the StatefulSet to 0, the same
way it retains the regular Service and both Secrets.

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

## Pausing a cluster

`spec.pause: true` scales the StatefulSet to 0 without deleting the
cluster: the Services, both Secrets (`<name>` auth, `<name>-cnf`
bootstrap), and every pod's PVC are all retained untouched. It's the
knob for "I don't need this control plane running right now but I'm not
ready to throw it away" — a maintenance window, a dev/staging cluster
parked overnight or over a weekend, or a cluster whose backends are
themselves down for a while. Since compute (CPU/memory requests) is
what a cluster billed by node-hours or a cluster autoscaler cares about,
and a paused `ProxySQLCluster` schedules zero pods, pausing is the cost
lever: storage (the PVCs) keeps billing at its usual, much smaller rate,
but the pod compute cost drops to zero for as long as it's paused.

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: proxysql
spec:
  replicas: 3
  pause: true
```

Pausing never touches `spec.replicas` — it only changes what the
StatefulSet's *actual* replica count is set to. Resuming is the mirror
image: flip `pause` back to `false` (or remove it) and the operator
restores the StatefulSet to `spec.replicas` with no other input needed.
Because persistence defaults to on, a resumed pod boots straight back
onto its existing PVC — including the local ProxySQL admin database — no
reconfiguration required. `ProxySQLConfig` pushes are skipped while
paused (there are no ready pods to push to either way) and the config's
`Ready` condition reports reason `ClusterPaused` instead of the generic
`NoReadyReplicas`, so it reads as intentional rather than an outage.

Status distinguishes two sub-states while `spec.pause: true` (mirroring
the pattern used by other ProxySQL/MySQL Kubernetes operators):

- **`Stopping`** (`phase`) / `Paused=False` (`reason: Stopping`) — the
  scale-down is still in flight; at least one replica is still ready.
- **`Paused`** (`phase`) / `Paused=True` (`reason: Paused`) — the
  StatefulSet has reached 0 ready replicas.

```bash
kubectl get pxc proxysql            # PHASE column: Stopping, then Paused
kubectl get pxc proxysql -o jsonpath='{.status.conditions[?(@.type=="Paused")]}'
```

The PodDisruptionBudget (when enabled) and any ServiceMonitor are left
as-is — they key off `spec.replicas`/`spec.metrics`, not the paused
StatefulSet, and are harmless with zero matching pods.

## Rolling updates

The pod template carries a `proxysql.com/cnf-checksum` annotation — a
hash over the rendered bootstrap cnf (and the Fluent Bit config when
logging is enabled). **Structural** cnf changes roll the pods: auth
Secret contents (admin/radmin credentials), protocol/port/interface
changes, metrics or web toggles, replica count (peer list), logging
settings. A change confined to `spec.variables` *values* is the
exception: the operator applies it to running replicas over the admin
port without a restart, falling back to a roll only when a variable
doesn't take at runtime or a key is added/removed — see
[runtime vs. restart
semantics](../reference/proxysqlcluster.md#configuration-changes-runtime-vs-restart).
Image and resource changes roll the pods the normal StatefulSet way.
Toggling `spec.tls.enabled` is a separate, pod-template-level trigger (the
`tls-init`/`tls-cleanup` init container and the `tls` volume appearing or
disappearing) — it rolls the pods once, the same as any other structural
change, but isn't part of the cnf-checksum mechanism above. What happens
*after* TLS is enabled — certificate renewal, cert-manager reissuing — is
a restart-free rotation instead; see [TLS](./tls.md#rotation). Pod
management is `Parallel`, so initial creation doesn't serialize,
while updates follow the StatefulSet rolling-update semantics.

Watch a rollout:

```bash
kubectl get pxc proxysql            # PHASE column: Running / Updating
kubectl rollout status sts/proxysql
```

## Next

- [Configuration](./configuration.md) — backends, users, query rules.
- [Operations](./operations.md) — status fields, troubleshooting, logs.
- [Tutorial 04 — high availability](../tutorials/04-high-availability.md).
