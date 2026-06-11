# Installing the operator

How to install, upgrade, and remove the ProxySQL operator with Helm, and
what to know before you do. This page is for platform engineers setting
up the operator itself; once it is running, see
[Managing clusters](./clusters.md) and the
[quickstart](../quickstart.md) for creating your first `ProxySQLCluster`.

## Prerequisites

- **Kubernetes ≥ 1.29.** The chart enforces this
  (`kubeVersion: ">=1.29.0-0"`). The floor exists because
  `spec.networking.tcpKeepalive` renders as pod-level
  `net.ipv4.tcp_keepalive_*` sysctls, which joined the Kubernetes
  safe-sysctl set in v1.29 (KEP-3105). On older clusters the kubelet
  rejects pods carrying them unless every node runs with
  `--allowed-unsafe-sysctls=net.ipv4.tcp_keepalive_*`.
- **Helm 3.**
- No cert-manager, no webhooks, no Prometheus Operator required. If the
  `monitoring.coreos.com` CRDs are absent, ServiceMonitor creation is
  skipped with a status condition instead of failing.

### Local development on kind

The repository ships kind tooling: `make kind-up` creates a local
cluster, `make operator-image-kind` builds the operator image and loads
it into kind, and `make e2e` runs the full end-to-end suite. Nothing
about the chart is kind-specific — the same install commands below work.

## Install

```bash
helm repo add proxysql https://proxysql.github.io/proxysql-on-k8s
helm install proxysql-operator proxysql/proxysql-operator \
  -n proxysql-system --create-namespace
```

This installs:

- the two CRDs, `proxysqlclusters.proxysql.com` (short name `pxc`) and
  `proxysqlconfigs.proxysql.com` (short name `pxcfg`),
- a single-replica `Deployment` running the controller manager,
- a `ClusterRole`/`ClusterRoleBinding` and `ServiceAccount` (see
  [Security](./security.md#rbac-the-chart-installs) for exactly what it
  can touch),
- a leader-election `Role` in the release namespace,
- an optional metrics `Service` for the manager's own metrics endpoint.

Verify:

```bash
kubectl -n proxysql-system get deploy
kubectl get crd proxysqlclusters.proxysql.com proxysqlconfigs.proxysql.com
```

The operator is **cluster-scoped**: it watches `ProxySQLCluster` and
`ProxySQLConfig` resources in every namespace. There is no
namespace-restricted mode. Install it once per Kubernetes cluster.

## Images and private registries

The manager image is `ghcr.io/proxysql/proxysql-operator`; the tag
defaults to the chart's `appVersion`. To pin or mirror it:

```yaml
# values.yaml for the operator chart
image:
  repository: my-registry.example.com/proxysql/proxysql-operator
  tag: "0.2.5"
  pullPolicy: IfNotPresent
imagePullSecrets:
  - name: my-registry-creds
```

The ProxySQL pods themselves use a separate image
(`proxysql/proxysql:3.0` by default), configured per cluster via
`ProxySQLCluster.spec.image` — including its own
`spec.imagePullSecrets`. See the
[ProxySQLCluster reference](../reference/proxysqlcluster.md).

For the full chart values surface (resources, scheduling, metrics TLS,
`configResyncInterval`, extra args), see the
[Helm values reference](../reference/helm-values.md).

## CRD handling

The CRDs live in the chart's `crds/` directory. Helm installs files
there on first `helm install` but — by Helm's own design — **never
touches them again**: `helm upgrade` does not update CRDs and
`helm uninstall` does not delete them.

Consequence: **on every operator upgrade, apply the CRDs yourself**
before (or right after) `helm upgrade`:

```bash
kubectl apply -f https://raw.githubusercontent.com/ProxySQL/proxysql-on-k8s/main/charts/proxysql-operator/crds/proxysqlcluster.yaml
kubectl apply -f https://raw.githubusercontent.com/ProxySQL/proxysql-on-k8s/main/charts/proxysql-operator/crds/proxysqlconfig.yaml
```

(Substitute a release tag for `main` to match the chart version you are
installing.) Skipping this is harmless when a release added no API
fields, and silently confusing when it did — new spec fields will be
stripped at admission until the CRDs are updated.

## Upgrades

```bash
helm repo update
kubectl apply -f <CRDs as above>
helm upgrade proxysql-operator proxysql/proxysql-operator -n proxysql-system
```

Existing `ProxySQLCluster` StatefulSets keep serving traffic during an
operator upgrade — the operator is a control plane, not a data path.
Pods only roll if a new operator version changes the generated pod
template or bootstrap config (the cnf checksum annotation triggers the
rolling restart; see [Managing clusters](./clusters.md#rolling-updates)).

### Upgrading from < v0.3.0: cnf ConfigMap → Secret

Operator versions before v0.3.0 rendered the bootstrap `proxysql.cnf`
into a **ConfigMap** named after the cluster. Because that file embeds
the admin/radmin/monitor passwords, it now lives in a **Secret** named
`<cluster>-cnf`. The migration is automatic: on the first reconcile
after upgrading, the operator

1. creates the `<cluster>-cnf` Secret,
2. deletes the legacy ConfigMap — but only if it carries the cluster's
   controller owner reference (a user-managed ConfigMap that merely
   shares the name is left alone),
3. rolls the pods onto the Secret-backed mount via the cnf-checksum
   annotation.

No action is required. This is also why the operator's RBAC still
includes `get/list/watch/delete` (but not create/update) on ConfigMaps.

## Running multiple replicas (HA) — and why not multiple installs

For operator high availability, scale the one install:

```yaml
replicaCount: 2
leaderElection:
  enabled: true   # the default
```

`leaderElection.enabled` passes `--leader-elect` to the manager; only
one replica reconciles at a time, the others stand by. Leader election
is only meaningful with `replicaCount > 1`.

Do **not** install the operator chart twice in the same Kubernetes
cluster. The operator watches CRs cluster-wide with cluster-scoped RBAC,
and two installs in different namespaces hold *separate* leader-election
leases — both would become active and fight over the same resources.
One install, scaled for HA, is the supported topology.

## Uninstall

```bash
helm uninstall proxysql-operator -n proxysql-system
```

What survives, deliberately or by Helm/Kubernetes semantics:

| Survives | Why |
| --- | --- |
| The CRDs | Helm never deletes `crds/`-installed CRDs. |
| Your `ProxySQLCluster` / `ProxySQLConfig` CRs | They are your data, not the chart's. |
| StatefulSets, Services, Secrets owned by clusters | Owned by the CRs, not by the operator Deployment. ProxySQL keeps serving traffic — unmanaged. |
| PVCs | StatefulSet volume-claim templates retain PVCs on deletion by default. |

Two ordering caveats:

- **Delete `ProxySQLConfig` resources before uninstalling the
  operator.** Each config carries the `proxysql.com/config-cleanup`
  finalizer, and only the operator removes it. A config deleted after
  the operator is gone hangs in `Terminating` forever; the escape hatch
  is annotating it with `proxysql.com/skip-cleanup: "true"` and
  reinstalling the operator, or removing the finalizer by hand. See
  [Configuration — deleting a config](./configuration.md#deleting-a-proxysqlconfig).
- **Deleting a CRD deletes all its CRs** — and with them every owned
  StatefulSet, Service, and Secret (PVCs still survive). Only remove the
  CRDs when you mean to remove every ProxySQL cluster.

## Next steps

- [Quickstart](../quickstart.md) — first cluster in five minutes.
- [Managing clusters](./clusters.md) — sizing, auth, persistence,
  exposure.
- [Tutorial 01 — your first cluster](../tutorials/01-first-cluster.md).
