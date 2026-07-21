# External Service Exposure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a `ProxySQLCluster` be reachable from outside Kubernetes: `spec.service.type` flips the main Service's type, and an opt-in `spec.service.external` block creates a curated `<cluster>-external` Service (data-plane ports by default, admin only behind `exposeAdmin: true`).

**Architecture:** Pure builders produce both Services from the defaulted spec; the reconciler diffs/applies via the existing `ensure*` pattern, plus an explicit delete path when the external block is disabled. `status.endpoints` gains the external address.

**Tech Stack:** Go (kubebuilder operator), controller-gen CRDs, envtest, bash e2e on kind.

**Spec:** `docs/superpowers/specs/2026-07-21-external-service-design.md` — the spec governs on any conflict.

> **Drift note:** this plan was written while PR #46 (`sqlStatements`) was in flight and before the runtime-reconfig feature. Line numbers are anchors, not gospel — re-locate by symbol name if they moved. The files this plan touches (`service.go`, cluster controller ensure-path, cluster types) are not touched by either of those features.

## Global Constraints

- Branch `feat/external-service` off `main`.
- Go commands run from `operator/` with `GOTOOLCHAIN=go1.25.10`.
- Never hand-edit `charts/proxysql-operator/crds/` — regenerate via `make sync-crds` from repo root.
- Builders are pure: no K8s client calls or I/O in `operator/internal/controller/builders/`.
- Default-true booleans use `*bool` (`AllocateLoadBalancerNodePorts`); default-false use plain `bool` (`Enabled`, `ExposeAdmin`).
- Admin port 6032 must NEVER appear on the external Service unless `exposeAdmin: true` — there is no `admin` key in the external `ports` map; the boolean is the only gate.
- PSA `restricted` posture untouched (no pod changes in this feature).

---

### Task 1: API types + CRD regeneration

**Files:**
- Modify: `operator/api/v1alpha1/proxysqlcluster_types.go` (extend `ServiceSpec`, ~line 224; add `ExternalServiceSpec`, `ExternalPortSpec`)
- Generated: `operator/config/crd/bases/`, `charts/proxysql-operator/crds/`, `zz_generated.deepcopy.go`

**Interfaces:**
- Produces (consumed by Tasks 2–4):

```go
// In ServiceSpec (existing struct — append fields):
	// Type sets the client-facing Service's type. All enabled ports ride
	// this Service, including admin — for a curated external entry point
	// use External instead.
	// +optional
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	Type corev1.ServiceType `json:"type,omitempty"`

	// External creates a second, curated Service "<cluster>-external" for
	// out-of-cluster clients. Disabled (or absent) removes it.
	// +optional
	External *ExternalServiceSpec `json:"external,omitempty"`

type ExternalServiceSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +optional
	// +kubebuilder:default=LoadBalancer
	// +kubebuilder:validation:Enum=NodePort;LoadBalancer
	Type corev1.ServiceType `json:"type,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// +optional
	LoadBalancerClass *string `json:"loadBalancerClass,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Cluster;Local
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`
	// AllocateLoadBalancerNodePorts defaults to true; *bool so explicit
	// false survives marshalling (repo convention).
	// +optional
	// +kubebuilder:default=true
	AllocateLoadBalancerNodePorts *bool `json:"allocateLoadBalancerNodePorts,omitempty"`
	// HealthCheckNodePort is only meaningful with externalTrafficPolicy:
	// Local. 0 lets the API server allocate.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=32767
	HealthCheckNodePort int32 `json:"healthCheckNodePort,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=SingleStack;PreferDualStack;RequireDualStack
	IPFamilyPolicy *corev1.IPFamilyPolicy `json:"ipFamilyPolicy,omitempty"`
	// +optional
	IPFamilies []corev1.IPFamily `json:"ipFamilies,omitempty"`
	// Ports selects which listeners ride the external Service. Empty map =
	// default set: mysql + pgsql (each only if its protocol is enabled).
	// Valid keys: mysql, pgsql, web, metrics. Admin is deliberately NOT a
	// valid key — see ExposeAdmin.
	// +optional
	Ports map[string]ExternalPortSpec `json:"ports,omitempty"`
	// ExposeAdmin adds the admin port (6032). The ProxySQL admin interface
	// on a network edge is a serious risk; keep this false unless you have
	// source-range and NetworkPolicy controls in place.
	// +optional
	ExposeAdmin bool `json:"exposeAdmin,omitempty"`
}

type ExternalPortSpec struct {
	// NodePort pins the node port (30000-32767); 0 = auto-allocate.
	// +optional
	// +kubebuilder:validation:Maximum=32767
	// +kubebuilder:validation:XValidation:rule="self == 0 || self >= 30000",message="nodePort must be 0 (auto) or in 30000-32767"
	NodePort int32 `json:"nodePort,omitempty"`
}
```

- [ ] **Step 1: Add the types above** to `proxysqlcluster_types.go`. Add a CEL or kubebuilder validation for the `Ports` map keys: `// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['mysql','pgsql','web','metrics'])",message="valid port keys: mysql, pgsql, web, metrics"` on the `Ports` field.
- [ ] **Step 2: Regenerate**: `cd operator && GOTOOLCHAIN=go1.25.10 make generate manifests && cd .. && make sync-crds`
- [ ] **Step 3: Build + test**: `cd operator && GOTOOLCHAIN=go1.25.10 go build ./... && GOTOOLCHAIN=go1.25.10 go test ./...` — expected PASS (inert field).
- [ ] **Step 4: Commit**: `git add operator/api operator/config charts/proxysql-operator/crds && git commit -m "feat(api): service.type + service.external exposure spec"`

---

### Task 2: Main-Service type in the builder (TDD)

**Files:**
- Modify: `operator/internal/controller/builders/service.go` (the `Service()` builder currently hardcodes `Type: corev1.ServiceTypeClusterIP`, ~line 38)
- Test: `operator/internal/controller/builders/service_test.go`

**Interfaces:**
- Consumes: `Spec.Service.Type` (Task 1).
- Produces: `Service()` returns the main Service with the spec'd type (default ClusterIP when unset — the CRD default makes unset impossible post-admission, but the builder must not panic on zero values in unit tests).

- [ ] **Step 1: Failing test** in `service_test.go` (follow the file's existing table/assert style):

```go
func TestService_TypeFromSpec(t *testing.T) {
	b := builderWithDefaults(t) // use the file's existing constructor helper
	b.Spec.Service.Type = corev1.ServiceTypeLoadBalancer
	svc := b.Service()
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("main service type = %q, want LoadBalancer", svc.Spec.Type)
	}
	b.Spec.Service.Type = ""
	if got := b.Service().Spec.Type; got != corev1.ServiceTypeClusterIP {
		t.Fatalf("unset type must default to ClusterIP, got %q", got)
	}
}
```
(Adapt the constructor call to the helper that actually exists in `service_test.go` — read it first.)
- [ ] **Step 2: Run** `GOTOOLCHAIN=go1.25.10 go test ./internal/controller/builders/ -run TestService_TypeFromSpec -v` — FAIL.
- [ ] **Step 3: Implement**: in `Service()`, replace the hardcoded type with a small helper `mainServiceType(spec)` returning `ClusterIP` for empty. The headless Service keeps `ClusterIP` + `ClusterIPNone` unconditionally.
- [ ] **Step 4: Run tests** — PASS. Full builders package green.
- [ ] **Step 5: Commit**: `git commit -am "feat(builders): main Service type from spec.service.type"`

---

### Task 3: External Service builder (TDD)

**Files:**
- Create: `operator/internal/controller/builders/external_service.go`
- Test: `operator/internal/controller/builders/external_service_test.go`

**Interfaces:**
- Consumes: `Spec.Service.External` (Task 1), existing `b.SelectorLabels()`, `b.Labels()`, and the port constants used by `servicePorts`.
- Produces: `func (b *Builder) ExternalService() *corev1.Service` — returns `nil` when `External == nil || !External.Enabled`; otherwise a Service named `b.Name + "-external"` in the cluster namespace.

Behavior to implement and test (each is a test case):
1. Nil/disabled → `nil`.
2. Defaults: enabled with empty `Ports` on a mysql+pgsql cluster → exactly ports `mysql` (6033) and `pgsql` (6133), type LoadBalancer.
3. Protocol filter: pgsql disabled in the cluster spec → only mysql, even if `ports: {pgsql: {}}` is listed (spec: exposed only when listed AND enabled).
4. Selection: `ports: {mysql: {}, web: {}}` → mysql + web only (no pgsql, since a non-empty map is explicit).
5. Admin gate: `exposeAdmin: true` adds port `admin` (6032); without it never present regardless of `Ports`.
6. NodePort pinning: `ports: {mysql: {nodePort: 30306}}` → `ServicePort.NodePort == 30306`.
7. Tuning passthrough: annotations, `LoadBalancerClass`, `ExternalTrafficPolicy`, `LoadBalancerSourceRanges`, `AllocateLoadBalancerNodePorts` (nil → true via pointer default), `HealthCheckNodePort`, `IPFamilyPolicy`, `IPFamilies` all land on the Service spec verbatim.
8. Selector equals `b.SelectorLabels()` (same pods as the main Service).

- [ ] **Step 1: Write the eight failing tests** (table-driven where natural, mirroring `service_test.go` style).
- [ ] **Step 2: Run** — FAIL (`ExternalService` undefined).
- [ ] **Step 3: Implement** `ExternalService()`; share port construction with `servicePorts` by extracting the per-listener `corev1.ServicePort` construction into small helpers rather than duplicating name/port/targetPort literals.
- [ ] **Step 4: Run** builders package — PASS.
- [ ] **Step 5: Commit**: `git commit -am "feat(builders): curated external Service builder"`

---

### Task 4: Reconciler wiring + endpoints + envtest

**Files:**
- Modify: `operator/internal/controller/proxysqlcluster_controller.go` (new `ensureExternalService` next to `ensureService` at ~line 291; call site in the reconcile sequence; endpoints at ~line 444)
- Modify: `operator/api/v1alpha1/proxysqlcluster_types.go` (`ClusterEndpoints`, ~line 449: add `External string` with doc comment) + regen
- Test: `operator/internal/controller/proxysqlcluster_controller_test.go` (envtest)

**Interfaces:**
- Consumes: `builders.ExternalService()` (Task 3).
- Produces: `ensureExternalService(ctx, owner, desired *corev1.Service) error` — applies when `desired != nil`; when `desired == nil`, deletes `<cluster>-external` if it exists (NotFound is success). `status.endpoints.external` = LB `Status.LoadBalancer.Ingress[0]` IP-or-hostname (empty until provisioned) or, for NodePort, `"nodePort"`-style host-less port list — follow the format `ClusterEndpoints` already uses for other surfaces.

- [ ] **Step 1: envtest cases (write first, watch fail):**
  - enabling external on an existing cluster creates `<cluster>-external` with the default data-plane ports and owner reference;
  - `exposeAdmin: true` adds 6032, flipping it back removes it;
  - setting `enabled: false` deletes the Service;
  - main Service type flips in place when `spec.service.type: NodePort` (same object UID before/after — no recreation).
- [ ] **Step 2: Implement** `ensureExternalService` (mirror `ensureService`'s diff/apply, plus the delete branch) and the endpoints addition.
- [ ] **Step 3: Full suite**: `GOTOOLCHAIN=go1.25.10 go test ./...` — PASS.
- [ ] **Step 4: Commit**: `git commit -am "feat(operator): reconcile external Service + endpoints"`

---

### Task 5: e2e scenario (kind)

**Files:**
- Create: `test/e2e/scenarios/external.sh`
- Modify: `test/e2e/run.sh` (append `scenario_external` to SCENARIOS)

Scenario (NodePort — LoadBalancer stays pending on kind):
1. Cluster with `service: {external: {enabled: true, type: NodePort, ports: {mysql: {nodePort: 30333}}}}`.
2. Assert `<cluster>-external` exists, type NodePort, carries exactly one port (3306-facing mysql at 6033, nodePort 30333), and **no** port 6032.
3. Connect from an in-cluster pod to `<node-internal-IP>:30333` (`kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}'`) with the mysql client and run `SELECT 1` through ProxySQL.
4. Patch `exposeAdmin: true`, poll until port 6032 appears; patch back, poll until gone.
5. Use `for _ in $(seq ...)` for poll loops (shellcheck SC2034 — unused loop vars are `_` in this suite).

- [ ] **Step 1: Write scenario following `drift.sh` conventions** (helpers: `log`, `fail`, `dump_ns`, `radmin_pw`, `admin_query`, `wait_config_synced` — check `lib.sh` signatures before use).
- [ ] **Step 2: Register in run.sh; `bash -n` + shellcheck both files.**
- [ ] **Step 3: Run `make kind-up && make e2e`** (or defer to CI if kind unavailable — note it).
- [ ] **Step 4: Commit**: `git commit -am "test(e2e): external Service exposure scenario"`

---

### Task 6: Documentation

**Files:**
- Modify: `docs/reference/proxysqlcluster.md` (field docs for `service.type`, `service.external.*` — match the file's field-doc format)
- Modify: `docs/user-guide/clusters.md` or new `docs/user-guide/` section "Exposing ProxySQL outside the cluster" (read `docs/README.md` index to place it; add to index)
- Modify: `docs/user-guide/security.md` (admin-exposure warning; recommend sourceRanges + NetworkPolicy; metrics-exposure note)

Content requirements: both paths explained (simple `type` flip vs curated external), default port policy, the `exposeAdmin` warning box (bold-lead-in paragraph style, not blockquotes), LB-pending semantics for `status.endpoints.external`, `ipFamilies` immutability caveat, one-Service-many-ports clarification (no per-port LB needed).

- [ ] **Step 1: Write all three doc changes; cross-link reference ↔ user guide.**
- [ ] **Step 2: `make lint template` (sanity) + commit**: `git commit -am "docs: external exposure reference + user guide + security notes"`

---

### Task 7: PR

- [ ] Push `feat/external-service`, open PR referencing the spec and this plan, verify all CI checks green (including e2e).
