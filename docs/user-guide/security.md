# Security

Where every credential lives and travels, what RBAC the operator needs,
what the pods are (and are not) allowed to do, which ports are exposed
where, and what's encrypted in transit versus what isn't. For the
security-relevant spec fields see the [ProxySQLCluster
reference](../reference/proxysqlcluster.md); for chart values see the
[Helm values reference](../reference/helm-values.md); for the full TLS
story (issuance tiers, rotation, backend trust) see the [TLS user
guide](./tls.md).

## The credential flow, end to end

```
auth Secret (<cluster> or spec.auth.secretName)
  admin-password ─┐
  radmin-password ─┼─► rendered into proxysql.cnf ─► <cluster>-cnf Secret
  monitor-password┘        (admin_credentials,          mounted read-only at
                            monitor_username/password)  /etc/proxysql/

user Secrets (passwordSecretRef per mysqlUsers/pgsqlUsers entry)
  └─► read by the ProxySQLConfig reconciler ─► INSERT INTO mysql_users/
      pgsql_users over the admin port (as radmin)
```

Three operator-level credentials per cluster, one Secret:

| Account | Used by | Notes |
| --- | --- | --- |
| `admin` | humans, inside the pod only | ProxySQL hardcodes `admin` to localhost — a remote login is rejected with "User 'admin' can only connect locally". |
| `radmin` | the operator (config pushes, runtime read-back) and ProxySQL Cluster sync | The remote-capable admin account. Use it for any admin access over the pod network. |
| `monitor` | ProxySQL's monitor module, *toward your backends* | The same user must exist on the backend databases for health/read_only checks — see [Backends](./backends.md#the-monitor-user). One Secret key drives both `mysql-monitor_password` and `pgsql-monitor_password`; end-to-end rotation runbook (MySQL dual passwords, PostgreSQL's no-dual-password window) in [Operations](./operations.md#rotating-the-monitor-credential). |

When the operator mints the Secret, passwords are random 32-character
hex strings (~128 bits of entropy), preserved across reconciles.

Rotation semantics — which credential rotations restart pods and which
apply at runtime — are covered in
[Operations](./operations.md#what-restarts-pods-what-doesnt) and
[Managing clusters](./clusters.md#auth-secrets).

Every credential above still travels as **plaintext MySQL wire protocol**
between the operator and a pod — and between a client and ProxySQL — unless
`spec.tls` is configured. Enabling it does not change *where* these
credentials live (the flow above is unaffected), only whether the
connections carrying them are encrypted; see [TLS](./tls.md) and [Network
exposure surface](#network-exposure-surface) below.

### Why the bootstrap cnf is a Secret, not a ConfigMap

ProxySQL must know its admin and monitor credentials at boot, so the
rendered `proxysql.cnf` necessarily embeds them. It therefore ships in
the `<cluster>-cnf` **Secret**, giving it the same RBAC surface as the
auth Secret itself. (Versions before v0.3.0 used a ConfigMap; upgrades
migrate automatically — see
[Installation](./installation.md#upgrading-from--v030-cnf-configmap--secret).)
User passwords from `passwordSecretRef` never touch the cnf at all —
they are pushed over SQL at runtime and exist only in ProxySQL's own
data directory.

The Fluent Bit sidecar config (`fluent-bit.conf`, second key of the same
Secret) deliberately contains no credentials: S3 keys and HTTP bearer
tokens reach the sidecar as env vars sourced from your Secrets via
`secretKeyRef`.

## The two auth schemas and their validation

`spec.auth.secretName` accepts either the operator schema
(`admin-password` / `radmin-password` / `monitor-password`, key names
overridable via `spec.auth.keys`) or the platform schema
(`username` / `password`, with `monitor-password` as an optional extra).
In the platform schema admin and radmin share the password, and a
username other than `admin`/`radmin` becomes an *additional*
remote-capable admin credential — so a platform's own login keeps
working against the admin port. A partial operator schema is an explicit
error, never a silent fall-through.

Because cnf values live inside double-quoted strings and
`admin_credentials` is split on `;`, credentials are validated **before
rendering**:

- usernames must match `^[A-Za-z0-9_.-]+$`;
- passwords must not contain `"`, `;`, or any control character.

Values violating this would corrupt the rendered config and could never
have worked; the operator rejects them up front
(`Degraded`/`AuthSecretError` on the cluster, `AdminSecretIncomplete` on
configs).

## RBAC the chart installs

One cluster-scoped role, bound to the operator's ServiceAccount
(`charts/proxysql-operator/templates/clusterrole.yaml`):

| Resource | Verbs | Why |
| --- | --- | --- |
| `proxysqlclusters`, `proxysqlconfigs` (+ status, finalizers) | full / status-patch / update | the CRDs it reconciles |
| `statefulsets`, `services`, `poddisruptionbudgets` | full | owned objects |
| `secrets` | create, get, list, patch, update, watch | auth + cnf Secrets; reading `passwordSecretRef` Secrets; **no delete** |
| `pods` | get, list, watch | discovering ready replicas to push config to |
| `configmaps` | get, list, watch, **delete** only | garbage-collecting the pre-v0.3.0 cnf ConfigMap; the operator no longer creates ConfigMaps |
| `servicemonitors` (monitoring.coreos.com) | full | optional ServiceMonitor |
| `events` | create, patch | event emission |

Be aware that **the operator can read Secrets in every namespace** —
that is inherent to resolving `passwordSecretRef`s cluster-wide, since
the manager cache is not namespace-scoped (a namespaced-only mode is not
implemented). Factor this into your threat model when deciding who may
create `ProxySQLConfig` resources: anyone who can create one in a
namespace can get a Secret value from that namespace pushed into a
ProxySQL `mysql_users` table they control. A leader-election Role
(leases) exists in the release namespace.

## Pod security: PSA `restricted`, everywhere

Every container the operator or charts produce — the `proxysql`
container, the `fluent-bit` sidecar, and the manager itself — runs:

- `runAsNonRoot: true`, uid/gid 999 (manager: non-root, no fixed uid)
- `allowPrivilegeEscalation: false`
- `readOnlyRootFilesystem: true`
- `capabilities.drop: [ALL]`
- `seccompProfile.type: RuntimeDefault`

Writable paths are explicit mounts only: `/var/lib/proxysql` (PVC or
emptyDir) and, with logging enabled, the bounded `/var/log/proxysql`
emptyDir shared with the sidecar. The e2e suite deploys into a
namespace enforcing `pod-security.kubernetes.io/enforce: restricted` to
keep this true. The `tcpKeepalive` sysctls stay within the safe-sysctl
set, so they don't break `restricted` either (Kubernetes ≥ 1.29).

`spec.podSecurityContext` / `spec.containerSecurityContext` allow
overrides for image quirks, but loosening the restricted posture is
explicitly against this project's design stance — if a change appears to
need it, find another way.

## Network exposure surface

By default every operator-managed Service is **ClusterIP** — nothing is
exposed outside the cluster unless you configure it. Two opt-in knobs
change that, both covered in detail in [Clusters — exposing the
Service](./clusters.md#exposing-the-service): flipping
`spec.service.type` to `NodePort`/`LoadBalancer` (puts every enabled port
of the *regular* Service on the edge, admin included, with no way to
carve it out), and `spec.service.external` (a second, curated Service,
`<cluster>-external`, whose default port set excludes admin and whose
`ports`/`exposeAdmin` fields decide exactly what rides it).

| Port | Surface | On regular Service | On headless Service | On external Service |
| --- | --- | --- | --- | --- |
| 6033 | MySQL protocol (data) | yes (when enabled) | yes | when enabled + selected (default set) |
| 6133 | PostgreSQL protocol (data) | yes (when enabled) | yes | when enabled + selected (default set) |
| 6032 | **admin** (MySQL wire) | yes, always | yes, always | **only with `exposeAdmin: true`** |
| 6070 | metrics (REST/Prometheus) | yes (when enabled) | no | only when listed under `ports` + enabled |
| 6080 | stats web UI (HTTPS, self-signed) | yes (when enabled) | no | only when listed under `ports` + enabled |

Points to note:

- The **admin port is always published cluster-internally** — the
  operator needs it. Remote admin logins require the `radmin`
  credential; `admin` only works from inside the pod.
- **Encryption in transit is opt-in, via `spec.tls`.** With `spec.tls`
  absent (the default), every listed port — including the operator's own
  admin connections — speaks plain wire protocol on the pod network; if
  your environment requires encryption-in-transit without configuring
  `spec.tls`, put a mesh/CNI encryption layer under it, and use
  NetworkPolicy to restrict who can reach 6032 (the operator and any DBA
  tooling are the only legitimate clients). With `spec.tls.enabled: true`,
  **all client-facing and admin ports serve TLS from the same
  certificate** — 6033/6133 and 6032 alike, there's no partial-enable —
  and the operator's own admin dials (config pushes, runtime reads,
  cluster-sync checks) switch to TLS along with everything else. See the
  [TLS user guide](./tls.md) for issuance tiers, rotation, and the
  distinction between this (frontend/admin) certificate and backend trust.
- Backend-side TLS for MySQL backends is per-server opt-in via
  `mysqlServers[].useSSL` in the `ProxySQLConfig`, verified against the
  trust bundle configured in `spec.tls.backend` — see [Backend TLS is a
  different PKI](./tls.md#backend-tls-is-a-different-pki). This is a
  **separate PKI** from the frontend/admin certificate above: don't assume
  enabling `spec.tls` alone encrypts ProxySQL's connections to your
  backend databases.

### Exposing anything externally: source ranges and NetworkPolicy

**Whenever you turn on `spec.service.external` (or flip `spec.service.type`
away from `ClusterIP`), restrict who can reach it.** Set
`service.external.loadBalancerSourceRanges` to the CIDR blocks that
legitimately need access — most cloud LB implementations enforce this at
the load balancer itself, before traffic reaches the cluster network. It
has no effect on `NodePort`: pair a `NodePort` external Service with a
NetworkPolicy (or an upstream firewall/security-group rule on the nodes)
instead, since any node's kernel will accept the connection regardless of
source. Either way, also add a NetworkPolicy scoped to the ProxySQL pods'
selector labels (`proxysql.com/cluster=<cluster>`) as defense in depth —
source ranges/security groups protect the edge, a NetworkPolicy protects
the pod if that edge is ever misconfigured or bypassed.

**Never expose admin (`service.external.exposeAdmin: true` or `service.type`
with admin riding along) without both of the above in place.** The admin
account can rewrite every routing rule, credential, and backend the proxy
knows — treat it like you would a database superuser account reachable
from the internet. Prefer keeping admin cluster-internal (the default
external port set already excludes it) and reaching it through
`kubectl port-forward`, a bastion, or an internal-only jump host instead of
exposing it directly, even behind source ranges.

**Exposing metrics externally is a softer risk, but still worth a
second thought.** The `/metrics`/REST endpoint (6070) discloses
operational detail — connection counts, query rates, backend topology —
that's information disclosure rather than a control-plane risk, but it's
still not something to hand to the open internet by default. Scope it with
`loadBalancerSourceRanges`/NetworkPolicy the same way, to your Prometheus
scraper's egress range rather than leaving it wide open.

## The monitor user

The monitor credential closes the loop between ProxySQL and your
backends: the bootstrap cnf sets `monitor_username="monitor"` with the
password from the auth Secret, and ProxySQL uses it for connect, ping,
and `read_only` checks against every backend server. Either create that
user on the backends, override the monitor credentials via
`mysqlVariables` to match an existing backend user, or disable the
monitor (`mysql-monitor_enabled: "false"`) — an unauthenticated monitor
eventually **shuns** otherwise-healthy backends. Setup details in
[Backends](./backends.md#the-monitor-user); symptoms in
[Operations](./operations.md#troubleshooting).
