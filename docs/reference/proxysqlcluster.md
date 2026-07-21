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
| Printer columns | `Replicas`, `Ready`, `Phase`, `Age` |

The operator reconciles a `ProxySQLCluster` into: a StatefulSet (named after
the cluster), a headless Service (`<name>-headless`), a client-facing Service
(`<name>`), a bootstrap-cnf Secret (`<name>-cnf`), an auth Secret (created as
`<name>` unless `spec.auth.secretName` references an existing one), an
optional PodDisruptionBudget (`<name>`), and an optional ServiceMonitor
(`<name>`).

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

**Persistence-toggle limitation.** The eventslog variables live in the
bootstrap cnf, and on a persistence-enabled cluster ProxySQL's own
`proxysql.db` wins over the cnf after the first start. Toggling
`queryLog` **off** therefore does not stop an already-running eventslog
there. To actually stop it, run on the admin port (or set via
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

The error is `spec.variables: "<key>" is reserved (bootstrap-structural)`.

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

## Configuration changes: runtime vs restart

Every change to `spec` that affects the bootstrap cnf goes through the same
two-step pipeline: (1) the `<cluster>-cnf` Secret is updated first, always;
(2) the operator then decides whether that change can be pushed to already-
running pods over the admin port, restart-free, or whether it requires a
StatefulSet rolling restart. Step 1 happens unconditionally, so even when
step 2 can't apply something at runtime, a *fresh* pod (new pod, or a pod
that does restart) always boots from the correct, already-updated cnf.

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
- **Adding or removing a variable key.** Removing a key is a restart *by
  design*, not a limitation: ProxySQL has no "unset" for a global variable,
  so a runtime apply could only leave the old value in place while the
  Secret says otherwise — silently wrong. A restart re-bootstraps from the
  cnf's variable set as a whole, which is the only way to actually drop a
  previously-set value.
- **Structural changes** — anything outside `spec.variables` values:
  listening ports/interfaces, admin/radmin credential rotation (the
  `admin_credentials` line), `replicas`/the `proxysql_servers` peer list,
  toggling `logging.queryLog` (which adds/removes the `eventslog_*` lines
  in `proxysql.cnf`), protocol enable/disable, and so on. These always
  roll every pod. Note the runtime-vs-restart classification diffs
  `proxysql.cnf` only.
- **Zero ready replicas at push time.** Nothing is pushed anywhere (there's
  nothing to dial); the cnf Secret is already updated, so pods bootstrap
  correctly once they come up. No restart is *triggered* by this path
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
| Structural cnf change | `Progressing=True`, reason `Rolling` | `RestartRequired: structural cnf change` |
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
| `endpoints` | `*ClusterEndpoints` | In-cluster DNS `host:port` per enabled surface, pointing at the regular Service: `mysql`, `pgsql`, `admin`, `web`, `metrics`. Empty field = surface disabled (`admin` is always set). Host form: `<name>.<namespace>.svc`. |
| `adminSecretName` | `string` | The auth Secret the operator wired in (created or referenced). |
| `conditions` | `[]metav1.Condition` | `Available`, `Progressing`, `Degraded`, `ServiceMonitorReady` — full reason inventory in the [status reference](status.md). |

### Phase semantics

| Phase | Meaning (derivation) |
|---|---|
| `Pending` | StatefulSet does not exist yet. |
| `Creating` | StatefulSet exists, 0 ready replicas. Deliberately coarse: a total outage of a previously running cluster also reads `Creating`. |
| `Running` | `readyReplicas == replicas` and the StatefulSet update revision equals the current revision (no rollout pending). |
| `Updating` | Any other state (partial readiness or revision mismatch). |
| `Degraded` | Auth-Secret resolution failed (missing external Secret, partial schema, invalid credential characters). |
| `Failed` | Reserved; never currently emitted — the operator reports `Degraded` for error states it can observe. |
