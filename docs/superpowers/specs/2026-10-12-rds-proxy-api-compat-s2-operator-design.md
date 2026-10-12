# RDS Proxy API Compatibility S2: Operator Delivery

**Status:** Draft  
**Owner:** Wazir Ahmed  
**Issue:** ProxySQL/proxysql-on-k8s#98  
**SaaS slice:** ProxySQL/proxysql-saas#2521

## Goal

Extend the existing ProxySQL custom resources and controller so one
`ProxySQLConfig` delivers native ProxySQL configuration and RDS-compatible
configuration to every ready replica.

The operator remains the only reconciler. It applies native state through the
ProxySQL admin port and RDS state through the AWS plugin API. It reports one
combined convergence result for the complete custom resource.

## Resource contract

Do not add a new custom resource or controller.

Add these fields to `ProxySQLCluster.spec`:

```yaml
serviceAccountName: proxysql-rds-workload
rds:
  region: us-east-1
  assumeRoleARN: arn:aws:iam::123456789012:role/ProxySQLRDS
  externalID: organization-stable-value
  vpcID: vpc-0123456789abcdef0
```

Add these fields to `ProxySQLConfig.spec`:

```yaml
version: 42
rds:
  config:
    schema_version: 1
    proxy: {}
    target_group: {}
    targets: []
    tags: []
    default_endpoint: {}
```

`version` is the SaaS cluster spec version. The operator observes and reports
that value. It does not create a separate RDS generation.

`rds.config` is the opaque S1 SaaS document. The Kubernetes API preserves its
nested fields. The operator is the first component that interprets and adapts
this document to the plugin contract.

## Workload identity

Copy `spec.serviceAccountName` to the StatefulSet pod template. The agent owns
creation of the ServiceAccount and its seller-role annotation.

The plugin uses the pod's seller workload identity to call STS
`AssumeRole` with `spec.rds.assumeRoleARN` and `spec.rds.externalID`. The AWS SDK
owns temporary credential refresh. The operator does not create IAM resources
or store customer access keys.

## Plugin image and bootstrap

SaaS selects the approved combined ProxySQL, AWS plugin, and web plugin image
through the existing image field. The operator does not hardcode an image.

For an RDS-enabled cluster, the operator:

- Enables the managed service and web listener on port 6080.
- Uses the combined image entrypoint instead of bypassing it with the standard
  `command: proxysql` override.
- Creates one internal SigV4 access key and secret for the cluster.
- Preserves the key pair in an operator-owned Kubernetes Secret.
- Adapts the SaaS RDS document into the plugin's version 1 bootstrap envelope.
- Stores the generated bootstrap file in an operator-owned Secret and mounts it
  read-only in every pod.
- Starts the image with `--aws-managed-bootstrap=<mounted-file>`.

The combined image entrypoint removes the bootstrap argument when
`proxysql.db` already exists. A restarted replica therefore restores the
plugin's persisted state instead of running bootstrap twice.

Bootstrap uses the stable CR identity for deployment/resource IDs and the
public proxy name from the RDS document. It uses MySQL writer hostgroup `1` and
reader hostgroup `2` for the S2 single-target-group model. The internal
credential receives only the plugin operations the operator calls.

The internal credential is not a SaaS API key. It is not returned in status,
events, logs, or user-facing APIs.

## Unified reconciliation

The existing `ProxySQLConfigReconciler` owns both configuration domains.

For each ready replica it:

1. Reads native runtime state through Admin SQL.
2. Reads RDS state with plugin Describe operations.
3. Excludes plugin-owned rows from the native drift comparison.
4. Produces one drift verdict for the complete custom resource.
5. Skips writes when the complete state already matches.

Plugin-owned MySQL server rows are rows in the writer and reader hostgroups
claimed by the RDS default target group. For the narrow S2 model, MySQL users
whose default hostgroup is one of those claimed hostgroups and which are not in
the native desired user set are treated as plugin-owned. Query rules remain
native-owned. A native user explicitly declared in `spec.mysqlUsers` remains
native-owned even when its default hostgroup is claimed.

This classification supports the S2 one-proxy, one-target-group workflow. It
is not a general ownership registry for every future enterprise plugin.

If either domain drifts, run one ordered cycle on that replica:

1. Clear mutable RDS state through the plugin RDS API.
2. Clear and apply native state through Admin SQL.
3. Apply the complete desired RDS state through plugin RDS API operations.
4. Read both domains again and verify the combined result.

The RDS clear step removes current targets, tags, and authentication bindings
through supported operations while preserving the bootstrapped proxy and
default target-group identity. The apply step maps the desired document to:

| Desired state | Plugin operation |
|---|---|
| Proxy settings and authentication | `ModifyDBProxy` |
| Connection-pool settings | `ModifyDBProxyTargetGroup` |
| Targets | `RegisterDBProxyTargets` |
| Tags | `AddTagsToResource` |
| Verification | Corresponding Describe and list operations |

All calls use HTTPS on each pod's port 6080 and the operator-owned SigV4
credential. Signing service is `rds`; signing region is `spec.rds.region`.
Requests use the pod certificate trust already established for the cluster.
Do not disable TLS verification.

ProxySQL cluster synchronization stays enabled. The operator still writes
every ready replica directly. Cluster sync is a backup propagation path, not
the source of replica convergence status.

## Status

Extend `ProxySQLConfig.status` with the desired product version and safe
per-replica results. Each result identifies the pod and reports:

- whether native and RDS state both match;
- the observed version;
- the last check time; and
- a safe reason when reconciliation fails.

Keep the existing aggregate fields. `syncedReplicas` counts replicas whose
complete native and RDS state matches the current `spec.version`.
`driftedReplicas` counts replicas where either domain differs or cannot be
verified. `observedVersion` advances only after the current complete state is
verified.

Conditions and messages must not contain passwords, Secret values, internal
signing material, signed requests, or fetched Secrets Manager content.

## Failure and recovery

- A partial replica failure is retried from the complete custom resource.
- Do not add rollback state. The next reconcile repeats the ordered cycle.
- A new or replaced replica is not converged until both domains verify.
- Periodic observation detects drift without a CR generation change.
- A stale status cannot mark a newer `spec.version` as converged.
- Deleting the last config clears RDS state through the API before native
  cleanup, unless the existing skip-cleanup escape hatch is set.

## Tests

Use package tests only for deterministic transformations and builders:

- CRD JSON preservation and generated schema.
- SaaS RDS document to bootstrap/API input adaptation.
- StatefulSet ServiceAccount, entrypoint, port, bootstrap mount, and unchanged
  rendering when RDS is absent.
- Internal credential Secret preservation.
- Combined fingerprint and ownership-aware drift classification.
- Safe per-replica status aggregation.

Use the repository's existing controller tests for reconciliation behavior.
Do not add a parallel behavioral harness.

The complete user workflow is tested later in the SaaS canonical E2E stack,
which builds this operator from the local checkout and installs it into Kind.

## Delivery

The operator PR is a cross-repository dependency of SaaS S2. It must publish
the generated CRDs and an operator image that the SaaS local stack can build
and deploy from source. The SaaS branch carries the matching agent CRD copies
and integration journey.

