# Restart-Free Runtime Reconfiguration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Variables-only `ProxySQLCluster` cnf changes reach all replicas via admin SQL (`UPDATE global_variables` + `LOAD/SAVE`) with zero pod restarts; anything else — or a variable that fails runtime read-back — still rolls the StatefulSet.

**Architecture:** The cnf Secret remains the persisted "applied" state and is always updated; the pod-template checksum annotation becomes the "booted" state and is only advanced when a restart is actually required. Classification is textual: a normalizer blanks runtime-appliable variable values in the rendered cnf, so `normalize(old) == normalize(new)` ⇔ variables-only change; the variable diff then comes from parsing the same two texts. Read-back from `runtime_global_variables` is the oracle for whether a variable took effect — no hardcoded restart-required list.

**Tech Stack:** Go (kubebuilder), text/template-rendered cnf, `proxysqlclient` (MySQL-wire admin), envtest, kind e2e.

**Spec:** `docs/superpowers/specs/2026-07-21-runtime-reconfig-design.md` (governs on conflict). Note: the spec's acceptance ("flip `mysql-max_connections` on `ProxySQLCluster`") requires a way to set arbitrary bootstrap variables from the cluster spec; Task 1 adds `spec.variables` for exactly that. Web/metrics/protocol toggles also change StatefulSet containerPorts, so they roll pods via the STS diff regardless — `spec.variables` is the primary fuel for the runtime path.

## Global Constraints

- Branch `feat/runtime-reconfig`. Base: `main` if PR #46 is merged (it carries the spec doc); otherwise `feat/sqlstatements`.
- Go commands from `operator/` with `GOTOOLCHAIN=go1.25.10`.
- `make sync-crds` for chart CRDs — never hand-edit `charts/proxysql-operator/crds/`.
- Builders stay pure (no I/O); `proxysqlclient.Sync`'s `Executor` interface untouched.
- Reserved cnf lines are NEVER runtime-applied and force the restart path when changed: any `*_credentials` line, `mysql_ifaces`, `interfaces` (mysql/pgsql), `datadir`, and the `proxysql_servers` block. Removing a variable also forces restart (runtime would silently keep the old value).
- With zero ready replicas, a variables-only change must NOT bump the restart annotation (pods bootstrap from the updated Secret) — this is also what makes the flow envtest-able.
- The e2e suite writes unused poll-loop variables as `for _ in ...` (shellcheck SC2034).

---

### Task 1: `spec.variables` — bootstrap global variables from the cluster spec

**Files:**
- Modify: `operator/api/v1alpha1/proxysqlcluster_types.go` (add `VariablesSpec`, field on `ProxySQLClusterSpec`)
- Modify: `operator/internal/controller/builders/proxysql_cnf.go` (template + `cnfData`, ~lines 32-129)
- Test: `operator/internal/controller/builders/builders_test.go`
- Generated: CRD bases + chart copy + deepcopy

**Interfaces:**
- Produces: `Spec.Variables VariablesSpec` with `Admin`, `MySQL`, `PostgreSQL map[string]string`. Keys are the FULL ProxySQL variable names (`admin-refresh_interval`, `mysql-max_connections`, `pgsql-…`) — same convention as `ProxySQLConfig.spec.*Variables`. Rendered into the matching cnf section with the domain prefix stripped (cnf sections use bare names).

- [ ] **Step 1: Failing builder tests**

```go
func TestBootstrapCnf_SpecVariablesRendered(t *testing.T) {
	b := newTestBuilder(t) // use builders_test.go's existing constructor helper
	b.Spec.Variables = proxysqlv1alpha1.VariablesSpec{
		MySQL: map[string]string{"mysql-max_connections": "700"},
		Admin: map[string]string{"admin-refresh_interval": "2500"},
	}
	cnf, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"max_connections=700", "refresh_interval=2500"} {
		if !strings.Contains(cnf, want) {
			t.Fatalf("cnf missing %q:\n%s", want, cnf)
		}
	}
}

func TestBootstrapCnf_SpecVariables_ReservedKeysRejected(t *testing.T) {
	b := newTestBuilder(t)
	b.Spec.Variables = proxysqlv1alpha1.VariablesSpec{
		Admin: map[string]string{"admin-admin_credentials": "x:y"},
	}
	if _, err := b.BootstrapCnf(nil); err == nil {
		t.Fatal("reserved key must be rejected")
	}
}
```

- [ ] **Step 2: Run** `GOTOOLCHAIN=go1.25.10 go test ./internal/controller/builders/ -run TestBootstrapCnf_SpecVariables -v` — FAIL (no `Variables` field).
- [ ] **Step 3: Implement**

API (`proxysqlcluster_types.go`):

```go
// VariablesSpec sets extra ProxySQL global variables in the bootstrap cnf.
// Keys are full variable names (admin-*, mysql-*, pgsql-*). Values render
// into the matching cnf section. Changes to runtime-settable variables are
// applied to running replicas via the admin interface without a restart;
// variables ProxySQL only honors at startup fall back to a rolling restart
// automatically (runtime read-back is the oracle).
type VariablesSpec struct {
	// +optional
	Admin map[string]string `json:"admin,omitempty"`
	// +optional
	MySQL map[string]string `json:"mysql,omitempty"`
	// +optional
	PostgreSQL map[string]string `json:"pgsql,omitempty"`
}
```

Add to `ProxySQLClusterSpec`: `// +optional` `Variables VariablesSpec \`json:"variables,omitempty"\``.
Add CEL on each map: `+kubebuilder:validation:XValidation:rule="self.all(k, k.startsWith('admin-'))"` (resp. `mysql-`, `pgsql-`) with messages, so wrong-domain keys fail at admission.

Builder: in `BootstrapCnf`, validate against a package-level reserved set before rendering:

```go
// reservedCnfKeys are bootstrap-structural: rendered by the template itself
// and never overridable or runtime-applied.
var reservedCnfKeys = map[string]struct{}{
	"admin-admin_credentials": {}, "admin-mysql_ifaces": {},
	"mysql-interfaces": {}, "pgsql-interfaces": {},
}
```

Reject with `fmt.Errorf("spec.variables: %q is reserved (bootstrap-structural)", k)`. Render via new template ranges at the END of each section (bare name: strip the `admin-`/`mysql-`/`pgsql-` prefix), sorted keys for deterministic output; `cnfData` gains `AdminExtra`, `MySQLExtra`, `PgSQLExtra map[string]string` (already-stripped, sorted at render via a `sortedKeys` template func or pre-sorted slice of pairs).

- [ ] **Step 4: Regenerate + full test**: `make generate manifests` (in operator/), `make sync-crds` (root), `GOTOOLCHAIN=go1.25.10 go test ./...` — PASS.
- [ ] **Step 5: Commit**: `feat(api): ProxySQLCluster.spec.variables bootstrap global variables`

---

### Task 2: cnf normalizer + variables parser (pure, TDD)

**Files:**
- Create: `operator/internal/controller/builders/cnf_variables.go`
- Test: `operator/internal/controller/builders/cnf_variables_test.go`

**Interfaces:**
- Consumes: rendered cnf text (the `BootstrapCnf` output format: sections `admin_variables={...}`, `mysql_variables={...}`, `pgsql_variables={...}` with one `key=value` per line, plus structural text outside them).
- Produces (used by Task 4):

```go
// ParseCnfVariables returns runtime-appliable variables from a rendered cnf,
// keyed by FULL variable name (admin-*, mysql-*, pgsql-*). Reserved keys
// (reservedCnfKeys) are excluded.
func ParseCnfVariables(cnf string) map[string]string

// NormalizeCnf replaces every runtime-appliable variable VALUE with the
// fixed placeholder "<runtime>" and returns the result. Reserved keys and
// all structural text are left verbatim, so:
//   NormalizeCnf(a) == NormalizeCnf(b)  ⇔  a and b differ only in
//   runtime-appliable variable values (same key set, same structure).
func NormalizeCnf(cnf string) string
```

Parsing rules (the format is our own template output — stable): a section starts at a line matching `^(admin|mysql|pgsql)_variables=` + `{`, ends at the matching `^}` line; inside, lines `^\s*([a-z0-9_]+)=(.*?)\s*$` are variables (strip trailing template artifacts none — values may be quoted strings; keep the raw value text). Full name = sectionPrefix + `-` + key. Everything else (including `proxysql_servers=(...)` blocks and `datadir`) is structural.

- [ ] **Step 1: Failing tests** — cover at minimum:

```go
func TestParseCnfVariables_FromRenderedTemplate(t *testing.T) // render BootstrapCnf with spec.variables set; assert parsed map contains mysql-max_connections="700", admin-refresh_interval="2500", and does NOT contain admin-admin_credentials or mysql-interfaces
func TestNormalizeCnf_VariablesOnlyChangeIsInvariant(t *testing.T) // two renders differing only in a variable value → equal normalized text
func TestNormalizeCnf_StructuralChangeDiffers(t *testing.T)        // renders differing in replicas (proxysql_servers block) or credentials → different normalized text
func TestNormalizeCnf_RemovedVariableDiffers(t *testing.T)         // render with a spec variable vs without → different normalized text (line removed)
```

Each test builds real cnf text via `newTestBuilder(t)` + `BootstrapCnf` — no hand-written cnf fixtures, so the parser is pinned to the actual template format.

- [ ] **Step 2: Run** — FAIL (functions undefined).
- [ ] **Step 3: Implement** `cnf_variables.go` (single scanner pass building both outputs; `NormalizeCnf` internally shares the section/line detection with `ParseCnfVariables`).
- [ ] **Step 4: Run** builders package — PASS.
- [ ] **Step 5: Commit**: `feat(builders): cnf variables parser + structural normalizer`

---

### Task 3: proxysqlclient — variable apply + runtime read-back (TDD)

**Files:**
- Modify: `operator/internal/proxysqlclient/sync.go` (export a wrapper over `syncVariables`, ~line 390)
- Modify: `operator/internal/proxysqlclient/runtime.go` (read-back)
- Test: `operator/internal/proxysqlclient/sync_test.go`, `runtime_test.go`

**Interfaces:**
- Consumes: existing `Executor`, `Querier` interfaces, `syncVariables(ctx, c, vars, domain)` and `quote()`.
- Produces (used by Task 4):

```go
// ApplyVariables pushes full-named variables ("mysql-max_connections") for
// one domain ("MYSQL"|"PGSQL"|"ADMIN") and loads+saves them. Thin exported
// wrapper over the sync path's variable step.
func ApplyVariables(ctx context.Context, c Executor, vars map[string]string, domain string) error {
	return syncVariables(ctx, c, vars, domain)
}

// ReadGlobalVariables returns variable_name→variable_value from
// runtime_global_variables for exactly the requested names.
func ReadGlobalVariables(ctx context.Context, q Querier, names []string) (map[string]string, error)
```

`ReadGlobalVariables` builds `SELECT variable_name, variable_value FROM runtime_global_variables WHERE variable_name IN (<quoted, sorted>)`, iterates `q.Query` rows (`[][]string`, cols `[name value]`). Empty `names` → return empty map without querying.

- [ ] **Step 1: Failing tests**

```go
// sync_test.go
func TestApplyVariables_EmitsUpdateLoadSave(t *testing.T) {
	rec := &recorder{}
	if err := ApplyVariables(context.Background(), rec, map[string]string{"mysql-max_connections": "700"}, "MYSQL"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"UPDATE global_variables SET variable_value='700' WHERE variable_name='mysql-max_connections'",
		"LOAD MYSQL VARIABLES TO RUNTIME",
		"SAVE MYSQL VARIABLES TO DISK",
	} {
		if !rec.seen(want) {
			t.Fatalf("missing %q in %v", want, rec.queries)
		}
	}
}

// runtime_test.go — use/extend the file's existing fake Querier pattern
func TestReadGlobalVariables_FiltersAndMaps(t *testing.T)  // fake returns rows; assert map + that the emitted query contains both quoted names
func TestReadGlobalVariables_EmptyNamesNoQuery(t *testing.T)
```

- [ ] **Step 2: Run** — FAIL. **Step 3: Implement.** **Step 4: Package tests PASS.**
- [ ] **Step 5: Commit**: `feat(proxysqlclient): ApplyVariables + runtime_global_variables read-back`

---

### Task 4: Cluster reconciler — restart-hash resolution + runtime apply

**Files:**
- Create: `operator/internal/controller/pod_discovery.go` (extract `discoverPodAddresses` from `proxysqlconfig_controller.go:432` into a package-level `func discoverPodAddresses(ctx context.Context, c client.Client, cluster *proxysqlv1alpha1.ProxySQLCluster, port int32) ([]string, error)`; config reconciler calls the shared one — pure refactor, both reconcilers are in package `controller`)
- Modify: `operator/internal/controller/proxysqlcluster_controller.go` (Reconcile ~lines 110-135; new `resolveRestartChecksum`; `updateStatus` Progressing reasons ~line 450)
- Test: `operator/internal/controller/proxysqlcluster_controller_test.go` (envtest), plus plain unit tests for the pure diff helper in a new `restart_checksum_test.go`

**Interfaces:**
- Consumes: `builders.CnfChecksum`, `builders.ParseCnfVariables`, `builders.NormalizeCnf` (Task 2), `proxysqlclient.New/ApplyVariables/ReadGlobalVariables` (Task 3), `pw.Radmin` from `resolvePasswords`.
- Produces: Reconcile computes the STS pod annotation via:

```go
// resolveRestartChecksum decides what proxysql.com/cnf-checksum the pod
// template should carry. oldCnf is the proxysql.cnf text from the Secret
// BEFORE this reconcile updated it ("" if the Secret didn't exist).
// prev is the current pod-template annotation ("" if no STS yet).
// Returns the annotation value plus a human summary for the Progressing
// condition ("", "RuntimeApplied: mysql-max_connections", or
// "RestartRequired: mysql-threads (runtime read-back mismatch)").
func (r *ProxySQLClusterReconciler) resolveRestartChecksum(
	ctx context.Context,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	oldCnf, newCnf, prev, newHash string,
	radminPassword string,
) (annotation string, summary string, err error)
```

Crash-safety marker: the Secret is updated before the SQL push (spec step 1), so
an operator crash between the two would make the next reconcile see
`oldCnf == newCnf` and lose the diff. To close that window the StatefulSet
carries an OBJECT-level annotation (`proxysql.com/vars-applied-hash`, on the
STS metadata — NOT the pod template, so writing it never restarts anything):
the SHA-256 of the sorted `ParseCnfVariables(newCnf)` map that was last
successfully runtime-applied or restart-applied. Helper:
`func varsHash(vars map[string]string) string` (sorted `k=v` lines, SHA-256 hex).

Algorithm (spec §Reconcile flow — implement exactly; `appliedVars` is the
current `proxysql.com/vars-applied-hash` value, `""` if absent):

```
newVars := ParseCnfVariables(newCnf); newVarsHash := varsHash(newVars)
if prev == "" or prev == newHash → return newHash, newVarsHash (fresh STS / booted on this cnf)
if oldCnf == "" → return newHash, newVarsHash (no prior Secret to diff against)
if NormalizeCnf(oldCnf) != NormalizeCnf(newCnf) → return newHash, newVarsHash, "RestartRequired: structural cnf change"
changed := {k: v for k,v in newVars if ParseCnfVariables(oldCnf)[k] != v}
if len(changed) == 0 && appliedVars == newVarsHash → return prev, newVarsHash, "" (already applied)
if len(changed) == 0 → changed = newVars   (crash recovery: Secret already updated,
                                            push the full set — idempotent UPDATEs)
addrs := discoverPodAddresses(ctx, r.Client, cluster, adminPort)
if len(addrs) == 0 → return prev, newVarsHash, "" (nothing running; pods bootstrap from the Secret)
for each addr: dial proxysqlclient.New(addr, "radmin", radminPassword);
    group `changed` by domain prefix → ApplyVariables per domain;
    ReadGlobalVariables(keys(changed)); collect mismatched keys
    dial/exec/query error → return "", "", "", err  (requeue; appliedVars annotation
                                                     NOT advanced, so retry re-pushes)
if mismatches → return newHash, newVarsHash, "RestartRequired: <sorted mismatched keys> (runtime read-back mismatch)"
return prev, newVarsHash, "RuntimeApplied: <sorted changed keys>"
```

`resolveRestartChecksum` therefore returns `(annotation, appliedVarsHash,
summary string, err error)`; the caller writes `appliedVarsHash` to the STS
object annotation in the same `ensureStatefulSet` pass.

Reconcile wiring: fetch the existing cnf Secret BEFORE `ensureCnfSecret` (capture `oldCnf`); fetch the existing STS annotation (`sts.Spec.Template.Annotations["proxysql.com/cnf-checksum"]`) before `ensureStatefulSet`; pass `resolveRestartChecksum`'s result to `b.StatefulSet(...)` instead of raw `CnfChecksum`. Surface `summary` (when non-empty) through `updateStatus` as the `Progressing` message with reason `RuntimeApplied` (ConditionFalse) or the existing rolling flow (restart case needs no new reason — the STS diff drives `Rolling`).

- [ ] **Step 1: Plain unit tests first** (`restart_checksum_test.go`, no envtest): extract the address-free branches into a pure helper `classifyCnfChange(oldCnf, newCnf, prev, newHash, appliedVars string) (verdict cnfVerdict, changed map[string]string)` with `cnfVerdict ∈ {verdictBootHash, verdictKeepPrev, verdictRuntimeTry, verdictStructural}`, and unit-test it for: fresh STS, unchanged cnf, structural change, variables-only change, removed variable (→ structural), already-applied (`appliedVars == newVarsHash` → keep-prev), and crash-recovery (`oldCnf == newCnf` but `appliedVars != newVarsHash` → runtime-try with the FULL variable set). `resolveRestartChecksum` then just wires verdicts to SQL I/O.
- [ ] **Step 2: envtest cases** (Ginkgo, no ProxySQL dialing happens because envtest has zero ready pods):
  - variables-only change (`spec.variables.mysql["mysql-max_connections"]: "700"→"701"`): cnf Secret content updates, STS pod annotation UNCHANGED;
  - structural change (e.g. `spec.replicas` 1→3 flips the `proxysql_servers` block, or an auth-password rotation): annotation changes to the new `CnfChecksum`;
  - legacy adoption: STS with an old-scheme annotation and an unchanged cnf keeps its annotation verbatim.
- [ ] **Step 3: Implement**; run `GOTOOLCHAIN=go1.25.10 go test ./...` — PASS (config-controller tests must stay green after the `discoverPodAddresses` extraction).
- [ ] **Step 4: Commit**: `feat(operator): restart-free runtime apply of variables-only cnf changes`

---

### Task 5: e2e scenario (kind)

**Files:**
- Create: `test/e2e/scenarios/runtimereconfig.sh`
- Modify: `test/e2e/run.sh` (SCENARIOS array, after `scenario_sqlstatements` if present, else after `scenario_drift`)

Scenario:
1. Cluster (1 replica, mysql only) with `spec: {variables: {mysql: {mysql-max_connections: "700"}}}`; wait Ready.
2. `admin_query` asserts `runtime_global_variables` shows 700; record `restarts0=$(kubectl get pod pxc-0 -o jsonpath='{.status.containerStatuses[0].restartCount}')` and `annot0` (pod annotation `proxysql\.com/cnf-checksum`).
3. Patch to `"701"`; poll (`for _ in $(seq 1 15)`) until `admin_query` returns 701.
4. Assert restartCount unchanged AND pod annotation unchanged AND the `Progressing` condition message contains `RuntimeApplied: mysql-max_connections`.
5. Structural change: patch `spec.protocols.mysql.port` to 6034; wait for rollout (pod recreated / annotation changed); assert cluster returns Ready.
6. Follow `drift.sh` conventions and `lib.sh` helper signatures (`radmin_pw NS CLUSTER`, `admin_query NS HOST PW SQL`, `dump_ns`, `log`, `fail`).

- [ ] **Step 1: Write scenario + register; `bash -n` + shellcheck.**
- [ ] **Step 2: `make kind-up && make e2e`** — all scenarios pass (defer to CI only if kind is unavailable, and say so).
- [ ] **Step 3: Commit**: `test(e2e): restart-free runtime reconfiguration scenario`

---

### Task 6: Documentation

**Files:**
- Modify: `docs/reference/proxysqlcluster.md` — `spec.variables` field docs (full-name key convention, CEL domain guards, reserved keys list) + a "Configuration changes: runtime vs restart" subsection describing the annotation/Secret split, read-back fallback, and the `Progressing` messages (`RuntimeApplied: …` / `RestartRequired: …`).
- Modify: `docs/user-guide/operations.md` — operational walk-through: what changes restart pods vs not; precedence note (`ProxySQLConfig.spec.*Variables` sync runs after and wins for keys set in both — same convergence as a restart); `ipFamilies`-style caveat does not apply here, but note removed variables force a restart by design.
- Modify: `docs/reference/status.md` (if it documents conditions) — the new `RuntimeApplied` Progressing reason.

- [ ] **Step 1: Write, matching each file's conventions (bold-lead-in callouts, no blockquotes).**
- [ ] **Step 2: `make lint template`; commit**: `docs: runtime reconfiguration semantics + spec.variables reference`

---

### Task 7: PR

- [ ] Push `feat/runtime-reconfig`; open PR titled `feat: restart-free runtime reconfiguration (spec.variables + SQL apply with read-back fallback)`, body linking the spec and summarizing the annotation/Secret state split, the read-back oracle, and validation evidence (unit + envtest + full kind e2e). Verify all CI checks green.
