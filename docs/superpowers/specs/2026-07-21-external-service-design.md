# External exposure: Service type + curated external Service

**Date:** 2026-07-21
**Status:** Approved design, pending implementation plan
**Decisions:** both paths — `spec.service.type` mutates the main Service AND an
opt-in `spec.service.external` creates a curated second Service · external
default is data-plane ports only, admin requires explicit `exposeAdmin: true` ·
full Service tuning surface (class, traffic policy, source ranges, nodePorts,
IP families, healthCheckNodePort)

## Background

The operator builds a single load-balanced ClusterIP Service (all enabled
ports: mysql 6033, pgsql 6133, admin 6032, web, metrics) plus a headless
Service for pod identity. That is a correct in-cluster single entry point,
but the Service type is hardcoded to `ClusterIP`
(`builders/service.go`), so there is no operator-managed path to expose
ProxySQL outside the cluster. Users must hand-craft their own
LoadBalancer/NodePort Service today. One Kubernetes Service carries multiple
ports, so no per-port LB is needed — the design question is *which* ports an
external entry point should carry, and that is a security question because of
the admin interface.

## API

`spec.service` (existing block: `annotations`,
`sessionAffinityTimeoutSeconds`) gains:

```yaml
service:
  type: ClusterIP                     # NEW: ClusterIP|NodePort|LoadBalancer (default ClusterIP)
  external:                           # NEW: opt-in second Service "<cluster>-external"
    enabled: false                    # plain bool — default-off, zero value is the default
    type: LoadBalancer                # LoadBalancer|NodePort (default LoadBalancer)
    annotations: {}                   # independent of the internal Service's annotations
    loadBalancerClass: ""
    externalTrafficPolicy: Cluster    # Cluster|Local
    loadBalancerSourceRanges: []
    allocateLoadBalancerNodePorts: true   # *bool (default-true convention)
    healthCheckNodePort: 0            # only meaningful with externalTrafficPolicy: Local
    ipFamilyPolicy: ""                # SingleStack|PreferDualStack|RequireDualStack
    ipFamilies: []                    # IPv4|IPv6
    ports:                            # presence = exposed; empty map = default set
      mysql: {nodePort: 0}            # nodePort optional, 30000-32767
      pgsql: {}
      web: {}
      metrics: {}
    exposeAdmin: false                # admin 6032 NEVER exposed externally without this
```

### Semantics

- **`service.type`** changes the type of the existing main Service. All
  enabled ports ride it, admin included — the simple path, with a documented
  footgun warning. Tuning fields for this path: the external block's tuning
  fields do NOT apply to the main Service; users needing per-LB tuning use
  the external block.
- **`service.external`** creates/maintains a second Service named
  `<cluster>-external`, owned by the ProxySQLCluster:
  - `ports` empty → default set = mysql + pgsql, each only if its protocol
    is enabled. `web`/`metrics` are exposed only when listed AND enabled in
    the cluster spec.
  - `admin` never rides the external Service unless `exposeAdmin: true` —
    an entry under `ports` alone is not sufficient (`admin` is not a valid
    `ports` key; the gate is the boolean, so a reviewer greps one field).
  - Disabling (`enabled: false` or removing the block) deletes the Service.
    Owner references GC it on cluster deletion regardless.
  - Annotation merge follows the internal Service's preserve-foreign-keys
    semantics.
- **Status:** `status.endpoints` gains the external entry — LoadBalancer
  ingress IP/hostname once provisioned (empty until then), or the node-port
  numbers for NodePort type.

### Validation

- CRD enums for both `type` fields, `externalTrafficPolicy`,
  `ipFamilyPolicy`, `ipFamilies` items.
- `nodePort` and `healthCheckNodePort`: 30000–32767 (0 = unset).
- `ipFamilies` mutation of an existing Service is rejected by the API
  server; the operator surfaces the update error in conditions rather than
  special-casing it (documented).

## Implementation

- **Builders stay pure** (repo invariant): `builders/service.go` gains the
  `type` plumbing for the main Service; new `builders/external_service.go`
  returns the desired external Service from the defaulted spec, reusing the
  `servicePorts` helper for port derivation with a filter for the selected
  external set.
- **Reconciler:** new `ensureExternalService` follows the existing ensure
  pattern — diff/apply when enabled, explicit delete when disabled (an
  absent-but-owned object would otherwise linger until cluster deletion).
- **RBAC:** no changes — services create/update/delete already granted.
- **Charts:** none — the operator, not the chart, owns these Services. The
  standalone `proxysql`/`proxysql-cluster` charts are out of scope.

## Security notes

- Admin (6032) externally exposed = ProxySQL admin interface on the
  network edge; `exposeAdmin` carries a doc warning box in the user guide
  and `docs/user-guide/security.md` gets a matching section
  (recommendation: keep admin in-cluster; use `loadBalancerSourceRanges`
  and NetworkPolicy when exposing anything).
- Metrics externally exposed is also called out (information disclosure),
  softer warning.

## Testing

- Builder unit tests: default port set (mysql-only cluster, pgsql-only,
  both), admin gate, nodePort pinning, tuning-field passthrough, main
  Service type change.
- envtest: external Service created with expected shape; deleted when
  disabled; admin absent without `exposeAdmin`, present with it;
  `status.endpoints` updated.
- e2e (kind): NodePort variant — object shape asserted, data port reachable
  via node IP:nodePort from an in-cluster pod, 6032 absent from the external
  Service. LoadBalancer stays `<pending>` on kind → shape-only coverage.
- Docs: `reference/proxysqlcluster.md` field docs; user-guide "Exposing
  ProxySQL outside the cluster" section; `security.md` update.

## Acceptance

- `service.type: LoadBalancer` flips the main Service in place (no Service
  recreation, ClusterIP retained).
- `service.external.enabled: true` with defaults yields
  `<cluster>-external` carrying exactly the enabled data-plane ports;
  adding `exposeAdmin: true` adds 6032; disabling removes the Service.
- All CRD validation enforced at admission; `make manifests && make
  sync-crds` clean; existing tests unchanged.
