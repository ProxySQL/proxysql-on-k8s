# proxysql-operator Helm chart values

Reference for every value of the `proxysql-operator` chart, which installs
the CRDs, the manager Deployment, and its RBAC. For installation walkthroughs
see the [installation user guide](../user-guide/installation.md). (The
standalone, operator-less `proxysql` and `proxysql-cluster` charts are
separate and not covered here.)

Chart facts:

| | |
|---|---|
| Chart | `proxysql-operator` |
| Kubernetes | `>= 1.29.0` (`kubeVersion` constraint) |
| CRDs | bundled under the chart's `crds/` directory — installed by Helm on first install, **not upgraded** by `helm upgrade` (standard Helm CRD handling; apply CRD updates manually on operator upgrades) |

## Values

### Deployment

| Value | Default | Description |
|---|---|---|
| `replicaCount` | `1` | Manager replicas. More than 1 only makes sense with `leaderElection.enabled`. |
| `image.repository` | `ghcr.io/proxysql/proxysql-operator` | Manager image. |
| `image.tag` | `""` | Empty defaults to the chart's `appVersion`. |
| `image.pullPolicy` | `IfNotPresent` | Pull policy. |
| `imagePullSecrets` | `[]` | Pull secrets for the manager pod. |
| `nameOverride` | `""` | Overrides the chart name in resource names. |
| `fullnameOverride` | `""` | Overrides the computed fullname entirely. |
| `resources` | requests `100m`/`128Mi`, limits `500m`/`256Mi` | Manager container resources. |
| `nodeSelector` | `{}` | Pod scheduling. |
| `tolerations` | `[]` | Pod scheduling. |
| `affinity` | `{}` | Pod scheduling. |
| `podAnnotations` | `{}` | Extra pod annotations. |
| `podLabels` | `{}` | Extra pod labels. |
| `extraArgs` | `[]` | Extra args appended to the manager command (after the chart-generated flags). |
| `extraEnv` | `[]` | Extra env vars for the manager container. |

### Controller behavior

| Value | Default | Description |
|---|---|---|
| `leaderElection.enabled` | `true` | Adds `--leader-elect` and renders the leader-election Role/RoleBinding (Leases + Events in the release namespace). Only meaningful with `replicaCount > 1`, but safe to leave on. |
| `configResyncInterval` | `""` | Go duration (e.g. `"30s"`, `"5m"`) passed as `--config-resync-interval`. Empty = operator default (**2m**). Bounds how long out-of-band runtime drift can persist before the ProxySQLConfig reconciler reads runtime state back and re-pushes drifted replicas — see the [status reference](status.md#the-hash-short-circuit-and-informed-resync). |

### Metrics and health (manager's own endpoints)

These are the **controller-manager's** controller-runtime endpoints, not the
ProxySQL metrics of managed clusters (those are `ProxySQLCluster
spec.metrics`).

| Value | Default | Description |
|---|---|---|
| `metrics.enabled` | `true` | When false, drops the `--metrics-bind-address` flag (metrics fully disabled), the `metrics` container port, and the metrics Service. |
| `metrics.secureServing` | `true` | Adds `--metrics-secure`: HTTPS with controller-runtime's authn/authz filter (self-signed cert unless one is provided via extraArgs `--metrics-cert-path`). |
| `metrics.port` | `8443` | Container port, wired into `--metrics-bind-address=:<port>`. |
| `metrics.service.type` | `ClusterIP` | Type of the `<fullname>-metrics` Service. |
| `metrics.service.port` | `8443` | Service port (targets the `metrics` container port). |
| `health.bindAddress` | `":8081"` | `--health-probe-bind-address`; the liveness (`/healthz`) and readiness (`/readyz`) probes target container port 8081. |

### Security

| Value | Default | Description |
|---|---|---|
| `podSecurityContext` | `runAsNonRoot: true`, `seccompProfile: RuntimeDefault` | PSA-restricted-compatible. |
| `containerSecurityContext` | `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities.drop: [ALL]` | PSA-restricted-compatible. |

### ServiceAccount and RBAC

| Value | Default | Description |
|---|---|---|
| `serviceAccount.name` | `""` | Empty = the chart creates and uses a ServiceAccount named `<fullname>`. Set = use a pre-existing ServiceAccount; the chart then creates **no** ServiceAccount (but still binds its RBAC to the named one). |
| `serviceAccount.annotations` | `{}` | Annotations on the chart-managed ServiceAccount (e.g. IRSA/workload-identity). Ignored when `serviceAccount.name` is set. |

There is **no `rbac.create` toggle**: the manager ClusterRole +
ClusterRoleBinding are always rendered. RBAC is always **cluster-scoped**
because the manager cache watches resources cluster-wide; a namespaced-only
mode is not implemented. The ClusterRole grants:

| Resources | Verbs |
|---|---|
| `proxysql.com`: proxysqlclusters, proxysqlconfigs (+ `/status`, `/finalizers`) | full CRUD / status update / finalizer update |
| `apps`: statefulsets; core: services; `policy`: poddisruptionbudgets; `monitoring.coreos.com`: servicemonitors | full CRUD |
| core: secrets | create, get, list, patch, update, watch (no delete) |
| core: pods | get, list, watch |
| core: configmaps | get, list, watch, **delete** only — garbage-collection of the legacy pre-v0.3.0 bootstrap-cnf ConfigMap; the operator no longer creates or updates ConfigMaps |
| core: events | create, patch |

When `metrics.enabled` and `metrics.secureServing` are both true (the
default), controller-runtime's `WithAuthenticationAndAuthorization` filter
guards the metrics endpoint, and the chart renders the RBAC that requires:

- The manager ClusterRole additionally gets `create` on
  `tokenreviews` (`authentication.k8s.io`) and `subjectaccessreviews`
  (`authorization.k8s.io`) — the manager needs these to validate the
  bearer token and permissions of each scrape request.
- A separate `<fullname>-metrics-reader` ClusterRole is rendered, granting
  `get` on the `/metrics` nonResourceURL. The chart does **not** bind it to
  anything, since it doesn't know which ServiceAccount does the scraping;
  bind it yourself to the scraper's ServiceAccount, e.g.:

  ```
  kubectl create clusterrolebinding <release>-metrics-reader \
    --clusterrole=<release>-proxysql-operator-metrics-reader \
    --serviceaccount=<scraper-namespace>:<scraper-serviceaccount>
  ```

Neither is rendered when `metrics.enabled: false` or
`metrics.secureServing: false` (nothing to authenticate/authorize against an
HTTP-only or disabled endpoint).

## Flags rendered into the manager command

For mapping values to behavior (`command: [/manager]`):

```
--leader-elect                                  # if leaderElection.enabled
--health-probe-bind-address=<health.bindAddress>
--config-resync-interval=<configResyncInterval> # only when non-empty
--metrics-bind-address=:<metrics.port>          # if metrics.enabled
--metrics-secure                                # if metrics.secureServing
<extraArgs...>
```

Any other manager flag (e.g. `--metrics-cert-path`, `--enable-http2`, zap
logging flags) can be supplied through `extraArgs`.
