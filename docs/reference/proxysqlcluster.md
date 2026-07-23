# ProxySQLCluster API reference

Complete field-by-field reference for the `ProxySQLCluster` custom resource
(`proxysql.com/v1alpha1`), the control-plane shape: pods, services, secrets,
ports, persistence, and the optional logging sidecar. For task-oriented
guidance see the [clusters user guide](../user-guide/clusters.md); for the
runtime configuration pushed to these pods see the
[ProxySQLConfig reference](proxysqlconfig.md).

| | |
|---|---|
| API group/version | `proxysql.com/v1alpha1` |
| Kind | `ProxySQLCluster` |
| Short name | `pxc` (`kubectl get pxc`) |
| Scope | Namespaced |
| Subresources | `status` |
| Printer columns | `Replicas`, `Ready`, `Phase`, `Age` (`Paused` at `-o wide` / priority 1) |

The operator reconciles a `ProxySQLCluster` into: a StatefulSet (named after
the cluster), a headless Service (`<name>-headless`), a client-facing Service
(`<name>`), an optional curated external Service (`<name>-external`, see
[External Service](#external-service)), a bootstrap-cnf Secret (`<name>-cnf`),
an auth Secret (created as `<name>` unless `spec.auth.secretName` references
an existing one), an optional PodDisruptionBudget (`<name>`), and an optional
ServiceMonitor (`<name>`).

## How defaults are applied

Two layers of defaulting exist and the tables below distinguish them:

- **CRD default** — a `+kubebuilder:default` marker; the API server persists
  the value into the stored object at admission.
- **Operator default** — applied in-process by the operator's `DefaultedSpec`
  on every reconcile, whether or not the CRD marker fired. These cover fields
  that have no CRD marker (or cannot have one, like `*bool` default-true
  fields and conditional port defaults).

When both exist they agree; the operator layer exists so behavior is
identical against API servers that did not apply the marker, and for fields
left at their zero value. Where only one layer applies, the Default column
says which.

## Spec

### Top level

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `replicas` | `*int32` | `3` (CRD + operator) | min 1 | Number of ProxySQL control-plane pods. |
| `pause` | `bool` | `false` | — | Scales the StatefulSet to 0 while retaining Services/Secrets/PVCs; never mutates `replicas`. See [Pausing a cluster](../user-guide/clusters.md#pausing-a-cluster). |
| `image` | `ImageSpec` | see [Image](#image) | — | ProxySQL container image. |
| `imagePullSecrets` | `[]LocalObjectReference` | `[]` | — | Pull secrets for the pod. |
| `auth` | `AuthSpec` | see [Auth](#auth) | — | Secret holding admin/radmin/monitor passwords. |
| `persistence` | `PersistenceSpec` | see [Persistence](#persistence) | — | Per-pod PVC at `/var/lib/proxysql`. |
| `protocols` | `ProtocolsSpec` | see [Protocols](#protocols) | — | Which client-facing listeners are on. |
| `resources` | `corev1.ResourceRequirements` | none | — | Requests/limits for the `proxysql` container. |
| `nodeSelector` | `map[string]string` | none | — | Pod scheduling. |
| `tolerations` | `[]corev1.Toleration` | none | — | Pod scheduling. |
| `affinity` | `*corev1.Affinity` | none | — | Pod scheduling. No default affinity is injected; unset means the pod has no affinity rules. |
| `podSecurityContext` | `*corev1.PodSecurityContext` | PSA-restricted (operator) | — | See [Security contexts](#security-contexts). |
| `containerSecurityContext` | `*corev1.SecurityContext` | PSA-restricted (operator) | — | See [Security contexts](#security-contexts). |
| `metrics` | `MetricsSpec` | see [Metrics](#metrics) | — | Prometheus exporter port + optional ServiceMonitor. |
| `podDisruptionBudget` | `PDBSpec` | see [PodDisruptionBudget](#poddisruptionbudget) | — | PDB for the StatefulSet. |
| `podAnnotations` | `map[string]string` | none | — | Merged onto the pod template. `proxysql.com/cnf-checksum` is [operator-reserved](annotations.md) — a user-supplied value for that key is always overwritten. |
| `podLabels` | `map[string]string` | none | — | Merged onto the pod template (selector labels always win for selection). |
| `service` | `ServiceSpec` | see [Service](#service) | — | Customizes the client-facing Service only. |
| `networking` | `NetworkingSpec` | see [Networking](#networking-tcpkeepalive) | — | TCP keepalive sysctls. |
| `logging` | `*LoggingSpec` | `nil` (sidecar off) | CEL rules, see [Logging](#logging) | Optional Fluent Bit query-log sidecar. |
| `variables` | `VariablesSpec` | see [Variables](#variables) | CEL + operator validation | Extra ProxySQL global variables baked into the bootstrap cnf. |
| `probes` | `ProbesSpec` | see [Probes](#probes) | — | Overrides the `proxysql` container's startup/readiness/liveness probes. |

### Image

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `image.repository` | `string` | `proxysql/proxysql` (CRD + operator) | — | Image repository. |
| `image.tag` | `string` | `"3.0"` (CRD + operator) | — | Image tag. |
| `image.pullPolicy` | `corev1.PullPolicy` | `IfNotPresent` (CRD + operator) | `Always`, `IfNotPresent`, `Never` | Pull policy. |

### Auth

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `auth.secretName` | `string` | `""` → operator creates a Secret named `<cluster-name>` | — | Name of an existing Secret. Empty = operator-managed: random 32-char hex passwords (~128 bits) minted on first reconcile, preserved afterwards; missing keys are backfilled. Set = externally managed: a missing Secret is a reconcile error (`Degraded`/`AuthSecretError`). |
| `auth.keys.adminPassword` | `string` | `admin-password` (CRD + operator) | — | Secret key holding the `admin` password. |
| `auth.keys.radminPassword` | `string` | `radmin-password` (CRD + operator) | — | Secret key holding the `radmin` password (the remote-capable admin account; ProxySQL restricts `admin` to localhost). |
| `auth.keys.monitorPassword` | `string` | `monitor-password` (CRD + operator) | — | Secret key holding the `monitor` password. |

#### Accepted Secret schemas and precedence

An externally managed Secret (and the cluster's admin Secret as read by the
`ProxySQLConfig` reconciler) is resolved against two schemas, in this order:

1. **Operator schema** — the three keys named by `auth.keys` (defaults
   above). Selected whenever the admin **or** radmin key is non-empty. All
   three keys are then required: a *partial* operator schema (admin or radmin
   present without the other two) is a hard error
   (`auth secret has a partial operator schema: missing key(s) ...`) rather
   than a silent fall-through — this prevents discarding explicitly
   configured keys. A Secret containing *only* the monitor key is **not**
   treated as partial; the monitor key doubles as the optional override for
   schema 2.
2. **Username/password schema** — keys `username` + `password` (both
   non-empty). `admin` and `radmin` share the password; `monitor` uses the
   same password unless the Secret also carries the monitor key
   (`monitor-password` by default). If `username` is anything other than
   `admin` or `radmin`, it is added to the bootstrap cnf as an **extra**
   remote-capable admin credential alongside `admin`/`radmin`.

A Secret matching neither schema fails with
`auth secret matches neither schema: need <adminKey>/<radminKey>/<monitorKey>, or username/password`.

#### Credential character validation

Credentials are rendered into the bootstrap `proxysql.cnf` (a double-quoted,
`;`-separated `admin_credentials` string), so the operator rejects values
that would corrupt it:

- **Passwords** (all schemas, all three roles): must not contain `"`, `;`,
  any control character below 0x20, or DEL (0x7f).
- **Username** (schema 2): must match `^[A-Za-z0-9_.-]+$`.

### Persistence

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `persistence.enabled` | `*bool` | `true` (operator; no CRD marker) | — | Mount a per-pod PVC at `/var/lib/proxysql`. Set `false` explicitly to use an `emptyDir` instead (required because the root filesystem is read-only). `*bool` so an explicit `false` survives serialization. |
| `persistence.size` | `resource.Quantity` | `1Gi` (operator, only when enabled) | — | PVC storage request. |
| `persistence.storageClass` | `*string` | none (cluster default class) | — | PVC `storageClassName`. |
| `persistence.accessModes` | `[]PersistentVolumeAccessMode` | `[ReadWriteOnce]` (operator, only when enabled) | — | PVC access modes. |

The PVC is created from a StatefulSet `volumeClaimTemplate` named `data`;
PVCs are retained when the cluster is deleted (standard StatefulSet
behavior).

### Protocols

`protocols` has four `ProtocolSpec` entries: `mysql`, `pgsql`, `admin`,
`web`. Each is:

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `enabled` | `*bool` | per-protocol, see below | — | Listener toggle. |
| `port` | `int32` | per-protocol, see below | 1–65535 | Listener port. |

Resolution rules (operator defaulting, applied every reconcile):

- An explicitly set `enabled` always wins over the port heuristic — **except
  `admin`**, which is forced on (`enabled: false` is ignored): the operator
  needs the admin port to push configuration.
- When `enabled` is unset (`nil`): `admin` and `mysql` default to **on**;
  `pgsql` and `web` default to **off**, but **a non-zero `port` implies
  enabled**.
- When a protocol resolves to enabled and `port` is 0, the default port is
  applied.

| Protocol | Default enabled | Default port | Notes |
|---|---|---|---|
| `protocols.admin` | always on (cannot be disabled) | `6032` | MySQL wire protocol regardless of data-plane protocols. Always exposed on both Services. |
| `protocols.mysql` | on | `6033` | MySQL data plane. |
| `protocols.pgsql` | off (`port` set ⇒ on) | `6133` | PostgreSQL data plane (ProxySQL 3.x). |
| `protocols.web` | off (`port` set ⇒ on) | `6080` | ProxySQL's built-in HTTPS stats web UI (admin `web_enabled`/`web_port`). Exposed on the regular Service only. |

All defaults here are operator-level; there are no CRD markers on
`ProtocolSpec`.

### Service

Applies to the client-facing (regular) Service only, never to the headless
Service.

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `service.annotations` | `map[string]string` | none | — | **Merged** onto the Service (labels are overwritten, annotations are not): spec keys win, annotations written by other controllers (cloud LB controllers etc.) are preserved. **Caveat:** a key you *remove* from this map lingers on the Service until removed by hand — the operator cannot distinguish a removed spec key from a foreign controller's key. |
| `service.sessionAffinityTimeoutSeconds` | `*int32` | none (no affinity) | 1–86400 | When set, enables `sessionAffinity: ClientIP` with this timeout. |
| `service.type` | `corev1.ServiceType` | `ClusterIP` (CRD + operator) | `ClusterIP`, `NodePort`, `LoadBalancer` | Sets the type of the **existing** regular Service in place — no new object is created, the ClusterIP is retained. **Every enabled port rides this Service, admin (6032) included** — there is no way to flip the type without also exposing admin on it. For a curated entry point that leaves admin off by default, use [`service.external`](#external-service) instead. |

Service port layout:

| Port name | Regular Service | Headless Service | Condition |
|---|---|---|---|
| `mysql` | yes | yes | `protocols.mysql` enabled |
| `pgsql` | yes | yes | `protocols.pgsql` enabled |
| `admin` | yes | yes | always |
| `metrics` | yes | no | `metrics.enabled` |
| `web` | yes | no | `protocols.web` enabled |

The headless Service sets `publishNotReadyAddresses: true` so pods are
DNS-resolvable during bootstrap.

### External Service

`service.external` (pointer; `nil` = disabled, the default) creates a
**second, independent** Service, `<cluster>-external`, for out-of-cluster
clients — independent of the regular Service's type and annotations. One
Kubernetes Service carries multiple ports, so exposing several listeners
externally never needs more than this one object (no per-port LoadBalancer).
Disabling it (`enabled: false`, or removing the block) deletes the Service;
an owner reference also garbage-collects it if the `ProxySQLCluster` itself
is deleted. It is **retained while `spec.pause: true`** — pause semantics
only retract the StatefulSet, not Services.

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `service.external.enabled` | `bool` | `false` | — | Plain `bool` (default-off, zero value is the default — repo convention differs from the `*bool` default-true fields elsewhere). |
| `service.external.type` | `corev1.ServiceType` | `LoadBalancer` (CRD + operator) | `NodePort`, `LoadBalancer` | `ClusterIP` is not offered here — an internal-only second Service would duplicate the regular one. |
| `service.external.annotations` | `map[string]string` | none | — | Merged the same way as `service.annotations`, but tracked **separately** — this Service's annotations do not inherit or share state with the regular Service's. |
| `service.external.loadBalancerClass` | `*string` | none | — | LoadBalancer-only; see the drop table below. |
| `service.external.externalTrafficPolicy` | `corev1.ServiceExternalTrafficPolicy` | apiserver default (`Cluster`) | `Cluster`, `Local` | Governs traffic arriving via the external (NodePort/LoadBalancer) address. Applies regardless of type. |
| `service.external.internalTrafficPolicy` | `*corev1.ServiceInternalTrafficPolicy` | apiserver default (`Cluster`) | `Cluster`, `Local` | Governs traffic arriving via the Service's cluster-internal ClusterIP; independent of `externalTrafficPolicy`. Applies regardless of type. |
| `service.external.loadBalancerSourceRanges` | `[]string` | none | — | LoadBalancer-only; see the drop table below. Recommended whenever `exposeAdmin` is set — see [Security](../user-guide/security.md#network-exposure-surface). |
| `service.external.allocateLoadBalancerNodePorts` | `*bool` | `true` (CRD + operator) | — | LoadBalancer-only; see the drop table below. `*bool` so explicit `false` survives serialization (repo convention). |
| `service.external.healthCheckNodePort` | `int32` | `0` (apiserver auto-allocates) | 0–32767 | LoadBalancer-only; see the drop table below. Only meaningful with `externalTrafficPolicy: Local`. |
| `service.external.ipFamilyPolicy` | `*corev1.IPFamilyPolicy` | none | `SingleStack`, `PreferDualStack`, `RequireDualStack` | Applies regardless of type. |
| `service.external.ipFamilies` | `[]corev1.IPFamily` | none | `IPv4`, `IPv6` | Applies regardless of type. **Immutable after the Service is created** — the apiserver rejects a mutation of `ipFamilies` on an existing Service. The operator does not special-case this: the rejection surfaces via the `Degraded`/`ExternalServiceError` condition (see [status reference](status.md)) and keeps retrying with the same rejected spec. To actually change families, toggle `enabled: false` then back to `true` (or delete/re-add the block) so the Service is recreated rather than mutated. |
| `service.external.ports` | `map[string]ExternalPortSpec` | empty map → default set | keys restricted to `mysql`, `pgsql`, `web`, `metrics` (CEL) | Selects which listeners ride the external Service. See port policy below. |
| `service.external.ports.<name>.nodePort` | `int32` | `0` (auto-allocate) | `0`, or `30000`–`32767` (CEL) | Pins the node port for that listener. |
| `service.external.exposeAdmin` | `bool` | `false` | — | Adds the admin port (6032). **Read the warning below before setting this.** |

**Port policy.** A listener rides the external Service only when it is
*selected* **and** its protocol is enabled in the cluster spec:

| Port | Selected when `ports` is empty (default set) | Selected when `ports` is non-empty | Also requires |
|---|---|---|---|
| `mysql` | yes | listed under `ports` | `protocols.mysql` enabled |
| `pgsql` | yes | listed under `ports` | `protocols.pgsql` enabled |
| `web` | no | listed under `ports` | `protocols.web` enabled |
| `metrics` | no | listed under `ports` | `metrics.enabled` |
| `admin` (6032) | no | no (`admin` is not a valid `ports` key — rejected at admission) | `exposeAdmin: true`, exclusively |

So `ports: {}` (or omitted) yields mysql + pgsql, each only if its protocol
is enabled — the external Service's default is **data-plane traffic only**.
`web`/`metrics` must be both listed under `ports` *and* enabled to appear;
listing a disabled protocol is a no-op, not an error.

**The `exposeAdmin` warning.** Setting `exposeAdmin: true` puts the
ProxySQL admin interface — the account that can rewrite every routing rule,
user, and backend the proxy knows about — on a network edge. It is gated by
this boolean alone: an `admin` entry under `ports` is never sufficient (CEL
rejects it, and the builder ignores it defensively even so), specifically
so a reviewer can grep this one field to find every externally admin-exposed
cluster. Combine it with `loadBalancerSourceRanges` and a NetworkPolicy — see
[Security](../user-guide/security.md#network-exposure-surface) for the full
recommendation.

**LoadBalancer-only fields, dropped on `NodePort`.** `loadBalancerClass`,
`loadBalancerSourceRanges`, `allocateLoadBalancerNodePorts`, and
`healthCheckNodePort` are only sent to the apiserver when
`service.external.type: LoadBalancer`. On `NodePort` the builder omits them
entirely — the apiserver otherwise rejects `allocateLoadBalancerNodePorts`
and `loadBalancerClass` outright ("may only be used when 'type' is
'LoadBalancer'"), and the other two carry LB-only semantics. This applies
even when the CRD default (`allocateLoadBalancerNodePorts: true`) would
otherwise populate the field.

**Apply failures.** A persistent apiserver rejection of the external
Service — a pinned `nodePort` colliding with another Service, the
`ipFamilies` immutability case above, or similar — does **not** wedge the
rest of the reconcile: the StatefulSet, PodDisruptionBudget, and
ServiceMonitor still apply on the same pass. The failure surfaces as
`Degraded=True`, reason `ExternalServiceError` (see [status
reference](status.md)), and clears on the next reconcile where the external
Service applies cleanly.

### Networking (tcpKeepalive)

`networking.tcpKeepalive` maps to the `net.ipv4.tcp_keepalive_*` pod
sysctls. Unset fields keep the node's kernel default. All three sysctls are
in the Kubernetes **safe-sysctl set since v1.29** (KEP-3105) and are admitted
under PSA `restricted`; on older clusters the kubelet rejects them unless
listed in `--allowed-unsafe-sysctls`.

| Field | Type | Default | Validation | Sysctl |
|---|---|---|---|---|
| `networking.tcpKeepalive.time` | `*int32` | kernel default | 1–32767 | `net.ipv4.tcp_keepalive_time` — idle seconds before probes start. |
| `networking.tcpKeepalive.interval` | `*int32` | kernel default | 1–32767 | `net.ipv4.tcp_keepalive_intvl` — seconds between probes. |
| `networking.tcpKeepalive.probes` | `*int32` | kernel default | 1–127 | `net.ipv4.tcp_keepalive_probes` — unanswered probes before drop. |

### Security contexts

Operator defaults (applied only when the corresponding field is entirely
unset; see the [security guide](../user-guide/security.md)):

`podSecurityContext` default:

```yaml
runAsNonRoot: true
runAsUser: 999
runAsGroup: 999
fsGroup: 999
seccompProfile:
  type: RuntimeDefault
```

`containerSecurityContext` default:

```yaml
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: [ALL]
```

These match PSA `restricted`. `tcpKeepalive` sysctls are appended to the pod
security context (on a copy; a user-supplied `podSecurityContext` keeps its
own fields).

### Metrics

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `metrics.enabled` | `*bool` | `true` (operator; no CRD marker) | — | Enables ProxySQL's REST API / Prometheus endpoint (`restapi_enabled` in the bootstrap cnf) and the `metrics` container/Service port. `*bool` so explicit `false` sticks. |
| `metrics.port` | `int32` | `6070` (CRD + operator) | — | Exporter port (`restapi_port`). |
| `metrics.serviceMonitor.enabled` | `bool` | `false` | — | Create a `monitoring.coreos.com/v1` ServiceMonitor. Plain `bool` (default-off). Requires `metrics.enabled`; a missing prometheus-operator CRD is surfaced as the non-fatal `ServiceMonitorReady` condition, never a reconcile failure. |
| `metrics.serviceMonitor.interval` | `string` | `"30s"` (CRD; operator falls back to `30s` if empty) | — | Scrape interval. |
| `metrics.serviceMonitor.scrapeTimeout` | `string` | `"10s"` (CRD; operator falls back to `10s` if empty) | — | Scrape timeout. |
| `metrics.serviceMonitor.labels` | `map[string]string` | none | — | Extra labels merged onto the ServiceMonitor (e.g. a Prometheus `release` selector). |

### PodDisruptionBudget

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `podDisruptionBudget.enabled` | `*bool` | `true` (operator; no CRD marker) | — | Create a PDB. Even when enabled, **no PDB is created when `replicas` ≤ 1** (and a previously created one is deleted). |
| `podDisruptionBudget.minAvailable` | `*intstr.IntOrString` | see below | — | Takes precedence over `maxUnavailable` when both are set. |
| `podDisruptionBudget.maxUnavailable` | `*intstr.IntOrString` | see below | — | Used only when `minAvailable` is unset. |

Default policy when neither is set: `minAvailable = replicas − 1` (so
`replicas: 2` → `minAvailable: 1`, `replicas: 3` → `minAvailable: 2`).

### Logging

`spec.logging` (pointer; `nil` = no sidecar) adds a Fluent Bit container
that ships ProxySQL's query log (eventslog) to stdout, S3, or an HTTP
collector. See the [query logging tutorial](../tutorials/05-query-logging.md).

CEL admission rules on the `logging` block:

| Rule | Message |
|---|---|
| `enabled: true` requires `queryLog: true` | `logging.queryLog is the only input; enable it or disable logging` |
| `sinkType: s3` requires the `s3` block | `sinkType=s3 requires the s3 block` |
| `sinkType: http` requires the `http` block | `sinkType=http requires the http block` |

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `logging.enabled` | `bool` | `false` | CEL above | Adds the `fluent-bit` sidecar container. |
| `logging.queryLog` | `bool` | `false` | CEL above | Enables ProxySQL's eventslog (all MySQL-protocol queries) via bootstrap-cnf variables (`eventslog_filename=/var/log/proxysql/queries`, `eventslog_default_log=1`, `eventslog_format=2`, `eventslog_filesize=52428800`). Currently the sidecar's only input. |
| `logging.sinkType` | `string` | `stdout` (CRD + operator) | `stdout`, `s3`, `http` | Destination. |
| `logging.s3` | `*S3SinkSpec` | — | required iff `sinkType: s3` | See below. |
| `logging.http` | `*HTTPSinkSpec` | — | required iff `sinkType: http` | See below. |
| `logging.image` | `string` | `fluent/fluent-bit:4.0.3` (CRD + operator) | — | Pinned Fluent Bit image; never `latest`. |
| `logging.resources` | `corev1.ResourceRequirements` | requests `50m`/`64Mi`, limits `200m`/`128Mi` (operator, per-list: only an entirely absent requests/limits list is defaulted) | — | Sidecar resources. |
| `logging.bufferSize` | `resource.Quantity` | `1Gi` (operator) | — | Bounds both the shared logs `emptyDir` (`sizeLimit`) and Fluent Bit's filesystem buffer (`storage.total_limit_size`, rounded up to whole MiB). On sink outage, logs buffer up to this bound, then the oldest chunks are dropped. |

`S3SinkSpec` (`logging.s3`):

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `bucket` | `string` | — | required, minLength 1 | Destination bucket. |
| `region` | `string` | — | required, minLength 1 | AWS region. |
| `prefix` | `string` | `/proxysql/<cluster-name>` (operator) | — | Object key prefix; keys are `<prefix>/%Y/%m/%d/%H%M%S-$UUID.jsonl`. |
| `endpoint` | `string` | none | — | Endpoint override for S3-compatible stores. |
| `credentialsSecretRef` | `LocalObjectReference` | — | required | Secret with keys `access-key-id` / `secret-access-key`, injected as `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` env vars. Credentials never appear in the rendered config. |

`HTTPSinkSpec` (`logging.http`):

| Field | Type | Default | Validation | Description |
|---|---|---|---|---|
| `host` | `string` | — | required, minLength 1 | Collector hostname. |
| `port` | `int32` | `443` when `tls: true`, else `80` (operator) | 1–65535 | Collector port. |
| `uri` | `string` | `/` (operator) | — | Request path. |
| `tls` | `bool` | `false` | — | HTTPS towards the collector. |
| `authTokenSecretRef` | `*LocalObjectReference` | none | — | Secret (key `token`) sent as `Authorization: Bearer <token>` via the `FLB_HTTP_TOKEN` env var. |

Sidecar mechanics (all verified in the builders):

- The sidecar is a regular container (not a native sidecar/initContainer),
  PSA-restricted (non-root 999/999, read-only rootfs, drop ALL,
  RuntimeDefault seccomp), with **no probes** — it never gates pod
  readiness.
- ProxySQL and Fluent Bit share a `logs` emptyDir at `/var/log/proxysql`
  (`sizeLimit: bufferSize`); Fluent Bit tails `queries*` there and keeps its
  position DB and buffer on the same volume.
- `fluent-bit.conf` rides in the cluster's `<name>-cnf` Secret as a second
  key; any change to either key rolls the pods via the
  [`proxysql.com/cnf-checksum`](annotations.md) annotation.

**Persistence-toggle limitation.** Toggling `queryLog` **off** *removes*
the `eventslog_*` lines from the bootstrap cnf, and the container's
`--reload` merge re-applies cnf lines over `proxysql.db` but never deletes
db entries absent from the cnf — so on a persistence-enabled cluster the
previously-saved eventslog settings survive the restart and the eventslog
keeps running. To actually stop it, run on the admin port (or set via
`ProxySQLConfig.spec.mysqlVariables`):

```sql
UPDATE global_variables SET variable_value='false'
  WHERE variable_name='mysql-eventslog_default_log';
LOAD MYSQL VARIABLES TO RUNTIME; SAVE MYSQL VARIABLES TO DISK;
```

### Variables

`spec.variables` adds extra ProxySQL global variables to the bootstrap cnf,
on top of the operator's own bootstrap-structural settings (credentials,
listening interfaces). It has three maps, one per ProxySQL variable domain:

| Field | Type | Validation | Description |
|---|---|---|---|
| `variables.admin` | `map[string]string` | CEL: every key must start with `admin-` | Rendered into the cnf's `admin_variables` block. |
| `variables.mysql` | `map[string]string` | CEL: every key must start with `mysql-` | Rendered into `mysql_variables`. Not rendered at all when `protocols.mysql` is disabled. |
| `variables.pgsql` | `map[string]string` | CEL: every key must start with `pgsql-` | Rendered into `pgsql_variables`. Not rendered at all when `protocols.pgsql` is disabled. |

Keys are ProxySQL's **full** variable names, prefix included (e.g.
`mysql-max_connections`, not `max_connections`) — the same convention as
`ProxySQLConfig`'s [variables maps](proxysqlconfig.md#variables-maps). Two
layers of validation apply:

- **CEL (admission)**: each map's keys must carry that map's domain prefix
  (`admin-`, `mysql-`, `pgsql-` respectively) — enforced by the
  `XValidation` rules above; a mismatched key is rejected at `kubectl apply`
  time.
- **Operator (reconcile)**: after stripping the domain prefix, the
  remaining variable name must match `^[a-z0-9_]+$` (lowercase snake_case);
  values may not contain double quotes (`"`), backslashes (`\`), control
  characters, or DEL — these could break out of the double-quoted
  `name="value"` line the operator renders. Anything else, including
  non-ASCII, is allowed. A violation fails the reconcile (rejected, not
  escaped) and retries with backoff; nothing is written until it's fixed.

**Reserved keys** — always rejected, because the operator owns these
values: it renders them from other spec fields (`spec.auth`,
`spec.protocols`, `spec.metrics`), and for the port/toggle keys the values
are additionally coupled to the StatefulSet (container ports, probe
wiring) — a cnf-only override would desync the pod spec:

| Key | Owned by |
|---|---|
| `admin-admin_credentials` | `spec.auth` (admin/radmin/extra credentials) |
| `admin-mysql_ifaces` | `spec.protocols.admin.port` |
| `mysql-interfaces` | `spec.protocols.mysql.port` |
| `pgsql-interfaces` | `spec.protocols.pgsql.port` |
| `mysql-monitor_username`, `mysql-monitor_password` | `spec.auth` (monitor credentials) |
| `pgsql-monitor_username`, `pgsql-monitor_password` | `spec.auth` (monitor credentials) |
| `admin-cluster_username`, `admin-cluster_password` | `spec.auth` (radmin; ProxySQL Cluster sync login) |
| `admin-restapi_enabled`, `admin-restapi_port` | `spec.metrics` (STS container-port coupling) |
| `admin-web_enabled`, `admin-web_port` | `spec.protocols.web` (STS container-port coupling) |
| `mysql-ssl_p2s_ca`, `mysql-ssl_p2s_cert`, `mysql-ssl_p2s_key` (and the `pgsql-` equivalents) | `spec.tls.backend` (backend TLS trust/client-cert paths) |

The error is `spec.variables: "<key>" is reserved (bootstrap-structural)`.

**Migration note:** hand-wired `ssl_p2s_*` variables in `spec.variables`
must migrate to `spec.tls.backend` (`caSecretName` /
`clientCertSecretName`) — they are rejected as reserved since the TLS
feature landed. `mysql-have_ssl` / `pgsql-have_ssl` remain user-settable
(e.g. `mysql-have_ssl: "false"` to disable the autogenerated-cert frontend
TLS on a cluster that doesn't use `spec.tls`); the unrendered
`ssl_p2s_capath`/`ssl_p2s_cipher`/`ssl_p2s_crl`/`ssl_p2s_crlpath` tuning
knobs also stay user-settable.

Keys the operator renders *by default* but does **not** reserve —
`mysql-threads`, `pgsql-threads`, `admin-cluster_check_*` and the other
cluster-sync tuning values, the `eventslog_*` family — are overridable: the
operator overlay-merges `spec.variables` over its own per-section defaults,
so each key renders exactly once and your value replaces the default
(libconfig rejects duplicate settings, so double-rendering would crashloop
the pod — the overlay guarantees it can't happen). Reserving the
monitor-credential keys only blocks *user overrides*: rotating the monitor
password through `spec.auth` still takes the restart-free runtime-apply
path — see [below](#configuration-changes-runtime-vs-restart).

### Probes

`spec.probes` overrides the `proxysql` container's `startupProbe`,
`readinessProbe`, and `livenessProbe`. Each field is a full
`corev1.Probe` — set one to **replace** the operator's default probe
entirely (handler, timings, thresholds, everything); there is no
per-field merging with the default (e.g. setting only
`probes.readiness.periodSeconds` still requires the rest of the probe,
including the handler, since the whole object replaces the default).
Leave a field unset to keep the operator's built-in default for it.

| Field | Type | Default | Description |
|---|---|---|---|
| `probes.startup` | `*corev1.Probe` | none (no startup probe) | ProxySQL boots fast with no external dependency to wait on, so the operator configures no default startup probe. |
| `probes.readiness` | `*corev1.Probe` | TCP check on the `admin` port, `initialDelaySeconds: 5`, `periodSeconds: 5`, `failureThreshold: 3` | Only verifies ProxySQL's admin interface is accepting connections. |
| `probes.liveness` | `*corev1.Probe` | TCP check on the `admin` port, `initialDelaySeconds: 15`, `periodSeconds: 10`, `failureThreshold: 3` | Same check, longer initial delay/period so transient admin-port hiccups don't trigger a restart. |

**Avoid backend-coupled readiness:** a custom `probes.readiness` that
depends on a MySQL/PostgreSQL backend being reachable *through* the proxy
(for example, an `exec` or `httpGet` probe that runs a query against a
backend) ties every replica's readiness to that backend's health. Because
every pod in the StatefulSet runs the same probe, a single backend outage
can flip every ProxySQL replica to `NotReady` simultaneously, pulling the
entire Service out of rotation — including for client traffic destined to
backends that are perfectly healthy. Prefer probing ProxySQL itself (the
default behavior) and let ProxySQL's own backend health checks and query
routing absorb backend failures instead of the kubelet.

Changing `spec.probes` changes the pod template, so it triggers a normal
StatefulSet rolling update — independent of the cnf runtime-vs-restart
pipeline described below.

## Configuration changes: runtime vs restart

Every change to `spec` that affects the bootstrap cnf goes through the same
two-step pipeline: (1) the `<cluster>-cnf` Secret is updated first, always;
(2) the operator then decides whether that change can be pushed to already-
running pods over the admin port, restart-free, or whether it requires a
StatefulSet rolling restart. Step 1 happens unconditionally, so even when
step 2 can't apply something at runtime, a pod with a *fresh* datadir (a
new pod on a new/ephemeral volume) boots from the correct, already-updated
cnf. **Persistence note:** the container starts with `--reload`, so on a
persistence-enabled cluster (the default) a pod that *restarts* onto its
existing PVC merges the cnf **over** `proxysql.db`: cnf values win for
keys present in both, db-only entries survive untouched, and the merged
state is saved back to disk. Restarts therefore converge PVC-backed pods
to the current cnf — including variables *added* since first boot — with
one exception: a line *removed* from the cnf keeps its old db value (the
merge never deletes). See
[operations](../user-guide/operations.md#what-restarts-pods-what-doesnt)
for the full behavior.

**What can be applied at runtime (no restart):** a change confined to
`spec.variables` values — an existing key's value changes, no keys are
added or removed. The operator diffs the old and new rendered cnf; if the
only difference is variable values within `admin`/`mysql`/`pgsql`
variable-domain lines, it connects to every **Ready** replica over the
admin port and, per changed domain:

```sql
UPDATE global_variables SET variable_value=<v> WHERE variable_name=<k>;
LOAD {ADMIN|MYSQL|PGSQL} VARIABLES TO RUNTIME;
SAVE {ADMIN|MYSQL|PGSQL} VARIABLES TO DISK;
```

It then reads `runtime_global_variables` back on that replica and compares
against the intended value. This read-back is the oracle for whether the
variable actually took effect at runtime — there is no hardcoded list of
"dynamic" vs "static" ProxySQL variables. If every changed variable reads
back as expected on every replica, **no pod restarts**: the pod template's
`proxysql.com/cnf-checksum` annotation is left unchanged, and the
StatefulSet-object-level `proxysql.com/vars-applied-hash` annotation is
updated to record the applied set (see [annotations
reference](annotations.md)).

**What always requires a rolling restart:**

- **Any variable ProxySQL doesn't honor at runtime.** If the read-back
  after `LOAD ... TO RUNTIME` doesn't match the intended value on any
  replica (e.g. `mysql-threads`, which ProxySQL only reads at startup), the
  operator falls back to a restart automatically — the `cnf-checksum`
  annotation changes and the mismatched variable names are named in the
  `Progressing` message.
- **Adding or removing a variable key.** An **added** key takes effect
  after the rollout on both fresh and PVC-backed pods (the `--reload`
  merge applies the new cnf line over `proxysql.db`). **Removing** a key
  is a restart *by design*, not a limitation: ProxySQL has no "unset" for
  a global variable, so a runtime apply could only leave the old value in
  place while the Secret says otherwise — silently wrong. With persistence
  disabled, the restart re-bootstraps from the cnf's variable set as a
  whole, dropping the previously-set value; with persistence enabled (the
  default) the old value survives in `proxysql.db` (the merge never
  deletes db-only entries — see the persistence note above) — verify on
  the admin port, or set the intended state explicitly via
  `ProxySQLConfig`.
- **Structural changes** — anything outside `spec.variables` values:
  listening ports/interfaces, admin/radmin credential rotation (the
  `admin_credentials` line), `replicas`/the `proxysql_servers` peer list,
  toggling `logging.queryLog` (which adds/removes the `eventslog_*` lines
  in `proxysql.cnf`), protocol enable/disable, and so on. These always
  roll every pod. Note the runtime-vs-restart classification diffs
  `proxysql.cnf` only.
- **Zero ready replicas at push time.** Nothing is pushed anywhere (there's
  nothing to dial); the cnf Secret is already updated, so a pod bootstraps
  the intended values once it comes up — a fresh datadir reads the cnf
  outright, and a PVC-backed pod merges the updated cnf over its existing
  `proxysql.db` via `--reload` (removed keys excepted — see the
  persistence note above). No restart is *triggered* by this path
  specifically, but nothing runtime-applies either until pods exist.

**The monitor-credential exception.** Credential rotation normally always
restarts, because `admin`/`radmin` live in the bootstrap-structural
`admin-admin_credentials` line. The `monitor` user's credentials are
different: `mysql-monitor_password` and `pgsql-monitor_password` are
ordinary variable lines rendered from `spec.auth` (reserved only against
`spec.variables` *overrides*, not bootstrap-structural for classification),
so rotating the monitor password is just a variable-value change like any
other — it goes through the runtime-apply path above and is restart-free.

**Progressing condition messages.** The reconciler surfaces the outcome in
the `Progressing` condition (see the [status reference](status.md)):

| Outcome | Condition | Message |
|---|---|---|
| Runtime apply succeeded | `Progressing=False`, reason `RuntimeApplied` | `RuntimeApplied: <sorted variable names>` |
| Runtime apply failed read-back / restart needed for a variable change | `Progressing=True`, reason `Rolling` | `RestartRequired: <sorted variable names> (runtime read-back mismatch)` |
| Structural `proxysql.cnf` change | `Progressing=True`, reason `Rolling` | `RestartRequired: structural cnf change` |
| Change confined to non-`proxysql.cnf` Secret keys (e.g. `fluent-bit.conf`) | `Progressing=True`, reason `Rolling` | `RestartRequired: structural cnf change (<keys>)` |
| Interrupted reconcile left a structural restart pending (`structural-applied-hash` mismatch) | `Progressing=True`, reason `Rolling` | `RestartRequired: structural change pending from interrupted reconcile` |
| Runtime push failed on a replica | `Degraded=True`, reason `RuntimeApplyError` | the push error, naming the replica address (retried on requeue; StatefulSet updates are not blocked) |
| Normal rollout, no variables-specific reason | `Progressing=True`, reason `Rolling` | `waiting for replicas` |

A `RuntimeApplied` outcome is reported even though `Progressing=False` —
nothing is rolling out, but it's worth surfacing that a restart-free change
just landed.

## Status

| Field | Type | Description |
|---|---|---|
| `observedGeneration` | `int64` | Last `.metadata.generation` reconciled. |
| `replicas` | `int32` | Desired replica count (from the defaulted spec). |
| `readyReplicas` | `int32` | Ready replicas of the underlying StatefulSet. |
| `updatedReplicas` | `int32` | Pods at the current StatefulSet revision. |
| `phase` | `string` | Coarse single-word projection for dashboards; see table below. Conditions remain the source of truth. |
| `endpoints` | `*ClusterEndpoints` | In-cluster DNS `host:port` per enabled surface, pointing at the regular Service: `mysql`, `pgsql`, `admin`, `web`, `metrics`, plus `external` — see below. Empty field = surface disabled (`admin` is always set). Host form: `<name>.<namespace>.svc`. |
| `adminSecretName` | `string` | The auth Secret the operator wired in (created or referenced). |
| `conditions` | `[]metav1.Condition` | `Available`, `Progressing`, `Degraded`, `Paused`, `ServiceMonitorReady` — full reason inventory in the [status reference](status.md). |

### `endpoints.external`

Unlike the rest of `endpoints` (a pure projection of the spec), `external`
depends on apiserver/cloud-provider allocations that happen asynchronously,
so it is read back from the **live** `<cluster>-external` Service on every
reconcile. Empty whenever `service.external` is absent/disabled, or the
Service was just created and hasn't been provisioned yet:

| `service.external.type` | Format | Notes |
|---|---|---|
| `LoadBalancer` | `"host:port"` | `host` is the first `status.loadBalancer.ingress[].ip`, falling back to `.hostname` when the provider only assigns one (e.g. AWS ELB). `port` is the external Service's **first** port — regardless of how many ports it carries, since they all share one host. **Empty until the cloud provider provisions the load balancer** — poll `status.endpoints.external` (or `kubectl get svc <cluster>-external`) rather than assuming it's populated right after `enabled: true` lands. |
| `NodePort` | comma-separated port list, e.g. `"30001,30002"` | The allocated node ports, in the external Service's port order; no host, since every cluster node's IP serves them. |

### Phase semantics

| Phase | Meaning (derivation) |
|---|---|
| `Pending` | StatefulSet does not exist yet. |
| `Creating` | StatefulSet exists, 0 ready replicas. Deliberately coarse: a total outage of a previously running cluster also reads `Creating`. |
| `Running` | `readyReplicas == replicas` and the StatefulSet update revision equals the current revision (no rollout pending). |
| `Updating` | Any other state (partial readiness or revision mismatch). |
| `Degraded` | Auth-Secret resolution failed (missing external Secret, partial schema, invalid credential characters). |
| `Failed` | Reserved; never currently emitted — the operator reports `Degraded` for error states it can observe. |
| `Stopping` | `spec.pause: true` and at least one replica is still ready — the StatefulSet is draining down to 0. Wins over every phase above. |
| `Paused` | `spec.pause: true` and 0 ready replicas — the StatefulSet has fully scaled to 0. Wins over every phase above. See [Pausing a cluster](../user-guide/clusters.md#pausing-a-cluster). |
