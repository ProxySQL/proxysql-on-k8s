# RDS Proxy API Compatibility S2 Operator Plan

**Spec:** `docs/superpowers/specs/2026-10-12-rds-proxy-api-compat-s2-operator-design.md`  
**Issue:** ProxySQL/proxysql-on-k8s#98

Implementation uses tests after production changes, as approved for S2.

## Phase 3: CRDs, identity, image, and bootstrap

1. Extend the typed CRDs with the cluster runtime block, ServiceAccount name,
   config version, opaque RDS document, and combined status fields.
2. Regenerate deepcopy code and both CRD copies with repository tooling.
3. Copy the ServiceAccount name to the pod template.
4. Add deterministic RDS document adapters and bootstrap rendering.
5. Create and preserve the internal SigV4 credential Secret.
6. Create the bootstrap Secret and mount it into every RDS-enabled pod.
7. Enable managed/web configuration and preserve the combined image entrypoint
   only for RDS-enabled clusters. Keep non-RDS golden output unchanged.
8. Add focused deterministic builder/adapter tests after implementation.
9. Build, run focused tests, inspect generated changes, self-review, commit,
   push, and update the PR phase table.

## Phase 4: Unified reconciliation and status

1. Add the signed HTTPS RDS client for the supported Query operations.
2. Add Describe-based RDS observation and desired-state comparison.
3. Classify plugin-owned hostgroup rows before native drift comparison.
4. Extend the existing reconcile path with the ordered RDS clear, native sync,
   RDS apply, and combined verification cycle.
5. Reconcile every ready replica and report safe per-replica results plus the
   existing aggregate fields.
6. Extend finalizer cleanup through the same RDS API gateway.
7. Add focused tests after implementation for signing, mapping, combined drift,
   apply order, partial failures, and status aggregation.
8. Build, run focused and repository tests, self-review, commit, push, and
   update both operator and SaaS PR phase tables.

