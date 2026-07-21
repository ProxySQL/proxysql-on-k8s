# `ProxySQLConfig.spec.sqlStatements` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a raw admin-SQL escape hatch (`spec.sqlStatements`) to `ProxySQLConfig`, executed write-to-all after structured config on every sync pass.

**Architecture:** The statements travel spec → `buildDesired` → `proxysqlclient.Desired.SQLStatements` → a new final step in `proxysqlclient.Sync`. They participate in the config hash automatically (the fingerprint is `json.Marshal(Desired)`), and reuse the existing error-aggregation and condition machinery — no new status fields.

**Tech Stack:** Go (kubebuilder operator), controller-gen CRD generation, plain `go test` fakes for the sync layer, bash e2e scenario on kind.

**Spec:** `docs/superpowers/specs/2026-07-21-sqlstatements-escape-hatch-design.md`

## Global Constraints

- Work on branch `feat/sqlstatements` off `main`.
- Go commands run from `operator/` with `GOTOOLCHAIN=go1.25.10` (repo requires Go 1.25+; system Go may be older).
- Never hand-edit `charts/proxysql-operator/crds/` — regenerate with `make sync-crds` from the repo root.
- Do not break the `Executor` interface in `proxysqlclient` — `sync_test.go`'s recording fake depends on it.
- Statements are opaque: no parsing, no rewriting, no implicit `LOAD ... TO RUNTIME`, no allow-listing.
- Deletion-finalizer cleanup pushes an empty `Desired{}`; an empty `SQLStatements` slice must be a no-op (nothing emitted).

---

### Task 1: API field + CRD regeneration

**Files:**
- Modify: `operator/api/v1alpha1/proxysqlconfig_types.go` (in `ProxySQLConfigSpec`, after the `PostgreSQLVariables` field)
- Generated: `operator/config/crd/bases/*.yaml`, `charts/proxysql-operator/crds/*.yaml`, `operator/api/v1alpha1/zz_generated.deepcopy.go`

**Interfaces:**
- Produces: `ProxySQLConfigSpec.SQLStatements []string` (json name `sqlStatements`), consumed by Task 3.

- [ ] **Step 1: Add the field**

Append to `ProxySQLConfigSpec` directly after the `PostgreSQLVariables` field:

```go
	// SQLStatements is raw admin SQL executed verbatim on every replica,
	// in order, after all structured config, on EVERY sync pass — including
	// on new or restarted replicas and after drift resyncs. Statements MUST
	// be idempotent. They are opaque to the operator: no implicit
	// LOAD/SAVE is appended, their effects are not drift-tracked, and
	// deletion cleanup does not reverse them. A statement that breaks admin
	// connectivity (e.g. changing admin credentials) locks the operator out
	// until a pod restart restores the cnf-based credentials.
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	SQLStatements []string `json:"sqlStatements,omitempty"`
```

- [ ] **Step 2: Regenerate deepcopy + CRDs + chart copy**

Run from repo root:
```bash
cd operator && GOTOOLCHAIN=go1.25.10 make generate manifests && cd .. && make sync-crds
```
Expected: `operator/config/crd/bases/proxysql.com_proxysqlconfigs.yaml` and the chart copy both gain `sqlStatements` (array of string, minLength 1).

- [ ] **Step 3: Build + existing tests still green**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 go build ./... && GOTOOLCHAIN=go1.25.10 go test ./...
```
Expected: PASS (field is inert so far).

- [ ] **Step 4: Commit**

```bash
git add operator/api/v1alpha1/ operator/config/crd/bases/ charts/proxysql-operator/crds/
git commit -m "feat(api): ProxySQLConfig.spec.sqlStatements raw admin-SQL field"
```

---

### Task 2: `proxysqlclient` — Desired field + sync step (TDD)

**Files:**
- Modify: `operator/internal/proxysqlclient/types.go` (end of `Desired` struct)
- Modify: `operator/internal/proxysqlclient/sync.go` (steps list in `Sync`, new `syncSQLStatements`)
- Test: `operator/internal/proxysqlclient/sync_test.go`

**Interfaces:**
- Consumes: `Executor.Exec(ctx, query, args...)` (existing).
- Produces: `Desired.SQLStatements []string`; `Sync` executes them last, in order, first failure aborts the remainder of the statements (but not earlier steps — they already ran).

- [ ] **Step 1: Write the failing tests**

Append to `sync_test.go`:

```go
// failOn wraps recorder and fails the first query containing failSubstr.
type failOn struct {
	recorder
	failSubstr string
}

func (f *failOn) Exec(ctx context.Context, q string, args ...any) error {
	if strings.Contains(q, f.failSubstr) {
		return fmt.Errorf("injected failure on %q", q)
	}
	return f.recorder.Exec(ctx, q, args...)
}

func indexOf(queries []string, substr string) int {
	for i, q := range queries {
		if strings.Contains(q, substr) {
			return i
		}
	}
	return -1
}

func TestSync_SQLStatements_VerbatimInOrderAfterVariables(t *testing.T) {
	rec := &recorder{}
	d := &Desired{
		AdminVariables: map[string]string{"admin-refresh_interval": "2000"},
		SQLStatements: []string{
			"UPDATE global_variables SET variable_value='250' WHERE variable_name='mysql-max_connections'",
			"LOAD MYSQL VARIABLES TO RUNTIME",
			"PROXYSQL FLUSH QUERY CACHE",
		},
	}
	if err := Sync(context.Background(), rec, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	iFlush := indexOf(rec.queries, "PROXYSQL FLUSH QUERY CACHE")
	iUpd := indexOf(rec.queries, "variable_name='mysql-max_connections'")
	iAdminApply := indexOf(rec.queries, "LOAD ADMIN VARIABLES TO RUNTIME")
	if iUpd == -1 || iFlush == -1 {
		t.Fatalf("statements not executed verbatim: %v", rec.queries)
	}
	if iUpd > iFlush {
		t.Fatalf("statements out of order: update at %d, flush at %d", iUpd, iFlush)
	}
	if iAdminApply == -1 || iUpd < iAdminApply {
		t.Fatalf("sqlStatements must run after structured variables (admin apply at %d, first statement at %d)", iAdminApply, iUpd)
	}
}

func TestSync_SQLStatements_FirstFailureAbortsRemainder(t *testing.T) {
	f := &failOn{failSubstr: "STATEMENT-B"}
	d := &Desired{SQLStatements: []string{
		"STATEMENT-A", "STATEMENT-B", "STATEMENT-C",
	}}
	err := Sync(context.Background(), f, d)
	if err == nil {
		t.Fatal("expected error from failing statement")
	}
	if !strings.Contains(err.Error(), "sqlStatements[1]") {
		t.Fatalf("error should name the failing statement index: %v", err)
	}
	if !f.seen("STATEMENT-A") {
		t.Fatal("statement before the failure must have executed")
	}
	if f.seen("STATEMENT-C") {
		t.Fatal("statement after the failure must NOT have executed")
	}
}

func TestSync_SQLStatements_EmptyIsNoOp(t *testing.T) {
	rec := &recorder{}
	if err := Sync(context.Background(), rec, &Desired{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if indexOf(rec.queries, "sqlStatements") != -1 {
		t.Fatalf("empty SQLStatements must add no queries")
	}
}
```

Add `"fmt"` to the test file's imports if not already present.

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 go test ./internal/proxysqlclient/ -run TestSync_SQLStatements -v
```
Expected: FAIL — `unknown field SQLStatements in struct literal`.

- [ ] **Step 3: Implement**

`types.go`, append to `Desired`:

```go
	// SQLStatements is raw admin SQL executed verbatim after all
	// structured sections. Opaque: no implicit LOAD/SAVE, not drift-tracked.
	SQLStatements []string
```

`sync.go`, append to the `steps` list in `Sync` (after the `admin_variables` step):

```go
		{name: "sql_statements", run: func() error { return syncSQLStatements(ctx, c, d) }},
```

New function (near `syncVariables`):

```go
// syncSQLStatements executes user-provided raw admin SQL in listed order.
// Unlike the table sections, the first failure aborts the remaining
// statements: order may carry dependencies (e.g. UPDATE then LOAD).
func syncSQLStatements(ctx context.Context, c Executor, d *Desired) error {
	for i, stmt := range d.SQLStatements {
		if err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("sqlStatements[%d]: %w", i, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the package tests**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 go test ./internal/proxysqlclient/ -v -run TestSync
```
Expected: all `TestSync_*` PASS, including the pre-existing ones.

- [ ] **Step 5: Commit**

```bash
git add operator/internal/proxysqlclient/
git commit -m "feat(proxysqlclient): execute Desired.SQLStatements as final sync step"
```

---

### Task 3: Controller plumbing + fingerprint coverage (TDD)

**Files:**
- Modify: `operator/internal/controller/proxysqlconfig_controller.go` (`buildDesired`, ~line 310)
- Test: `operator/internal/controller/fingerprint_test.go` (new, plain Go test — no envtest needed)

**Interfaces:**
- Consumes: `ProxySQLConfigSpec.SQLStatements` (Task 1), `Desired.SQLStatements` (Task 2), existing `syncFingerprint(d *proxysqlclient.Desired, addrs []string) string`.
- Produces: statements flow into every `applyToReplicas` push; hash short-circuit busts on edit. (Cleanup path at `proxysqlconfig_controller.go:579` already pushes `&proxysqlclient.Desired{}` — empty slice, no-op, nothing to change.)

- [ ] **Step 1: Write the failing test**

Create `operator/internal/controller/fingerprint_test.go`:

```go
package controller

import (
	"testing"

	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
)

func TestSyncFingerprint_ChangesWithSQLStatements(t *testing.T) {
	addrs := []string{"10.0.0.1:6032"}
	base := syncFingerprint(&proxysqlclient.Desired{}, addrs)
	withStmt := syncFingerprint(&proxysqlclient.Desired{
		SQLStatements: []string{"PROXYSQL FLUSH QUERY CACHE"},
	}, addrs)
	if base == withStmt {
		t.Fatal("adding sqlStatements must change the sync fingerprint")
	}
	edited := syncFingerprint(&proxysqlclient.Desired{
		SQLStatements: []string{"PROXYSQL FLUSH MYSQL QUERY CACHE"},
	}, addrs)
	if withStmt == edited {
		t.Fatal("editing a statement must change the sync fingerprint")
	}
}
```

- [ ] **Step 2: Run it — it should already PASS**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 go test ./internal/controller/ -run TestSyncFingerprint -v
```
Expected: PASS (the fingerprint is `json.Marshal(Desired)`, and Task 2 added an exported field). This test pins the behavior so a future `json:"-"` tag or hand-rolled hash can't silently drop statements. If it FAILS, the field isn't reaching the marshal — fix Task 2 before continuing.

- [ ] **Step 3: Plumb the spec field into buildDesired**

In `buildDesired`, add to the `&proxysqlclient.Desired{...}` literal (the struct at ~line 310):

```go
		SQLStatements: append([]string(nil), cfg.Spec.SQLStatements...),
```

- [ ] **Step 4: Full operator test suite**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 go test ./...
```
Expected: PASS (envtest suite downloads binaries automatically on first run).

- [ ] **Step 5: Commit**

```bash
git add operator/internal/controller/
git commit -m "feat(operator): push ProxySQLConfig.spec.sqlStatements to all replicas"
```

---

### Task 4: e2e scenario on kind

**Files:**
- Create: `test/e2e/scenarios/sqlstatements.sh`
- Modify: `test/e2e/run.sh` (SCENARIOS array, line ~37)

**Interfaces:**
- Consumes: `lib.sh` helpers used by every scenario: `log`, `fail`, `dump_ns`, `radmin_pw <ns> <cluster>`, `admin_query <ns> <cluster> <pw> <sql>`, `wait_config_synced <ns> <cfg> <want> <timeout>`.

- [ ] **Step 1: Write the scenario**

Create `test/e2e/scenarios/sqlstatements.sh`:

```bash
#!/usr/bin/env bash
# Scenario: spec.sqlStatements raw admin SQL escape hatch.
#  1. Statements execute on the replica (UPDATE + LOAD visible in runtime).
#  2. Editing a statement re-syncs (lastAppliedHash advances, new value lands).
# Uses mysql-max_connections, which the structured config below does NOT set,
# so any runtime effect is attributable to sqlStatements alone.

scenario_sqlstatements() {
  local ns=e2e-sqlstmt
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: false}}
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  sqlStatements:
    - "UPDATE global_variables SET variable_value='777' WHERE variable_name='mysql-max_connections'"
    - "LOAD MYSQL VARIABLES TO RUNTIME"
YAML
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin out hash0 i
  radmin="$(radmin_pw "$ns" pxc)"
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "777" ]] || { fail "sqlStatements did not apply (max_connections='$out', want 777)"; dump_ns "$ns"; return 1; }
  log "sqlstatements: raw SQL applied (runtime max_connections=777)"

  hash0="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')"
  kubectl -n "$ns" patch proxysqlconfig pxcfg --type=json \
    -p='[{"op":"replace","path":"/spec/sqlStatements/0","value":"UPDATE global_variables SET variable_value='\''778'\'' WHERE variable_name='\''mysql-max_connections'\''"}]' >/dev/null
  for i in $(seq 1 15); do
    [[ "$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')" != "$hash0" ]] && break
    sleep 4
  done
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "778" ]] || { fail "edited statement did not re-sync (max_connections='$out', want 778)"; dump_ns "$ns"; return 1; }
  log "sqlstatements: statement edit re-synced (runtime max_connections=778)"
}
```

- [ ] **Step 2: Register it**

In `test/e2e/run.sh`, add `scenario_sqlstatements` to the `SCENARIOS` array after `scenario_drift`:

```bash
SCENARIOS=(scenario_mysql scenario_postgres scenario_multireplica scenario_drift scenario_sqlstatements scenario_psa scenario_delete scenario_rotate scenario_platform scenario_logging)
```

- [ ] **Step 3: Run the e2e suite (requires docker + ~15 min)**

```bash
make kind-up && make e2e
```
Expected: `scenario 'sqlstatements' passed` in the output, all other scenarios still green. If kind can't run locally, note it and rely on the `operator / e2e on kind` CI job on the PR.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/
git commit -m "test(e2e): sqlStatements escape-hatch scenario"
```

---

### Task 5: Documentation

**Files:**
- Modify: `docs/reference/proxysqlconfig.md` (field reference)
- Modify: `docs/user-guide/configuration.md` (usage section with warnings)

**Interfaces:** none (docs only). Match each file's existing heading/style conventions.

- [ ] **Step 1: Reference entry**

In `docs/reference/proxysqlconfig.md`, add a `sqlStatements` section alongside the other spec fields (follow the file's existing field-doc format), covering: type (`[]string`, optional), execution order (after all structured config), verbatim/no-implicit-LOAD, per-pass re-execution, first-failure-aborts-remainder with `Degraded`/`PartialSync` surfacing, participation in `lastAppliedHash`, not drift-tracked, not reversed on deletion.

- [ ] **Step 2: User-guide section**

In `docs/user-guide/configuration.md`, add a "Raw SQL statements (escape hatch)" section with: the YAML example from the spec, an idempotency warning ("statements re-run on every sync pass, on new replicas, and after drift resyncs — write them so re-execution is harmless"), and a lockout warning ("statements that change admin credentials will lock the operator out until a pod restart restores the cnf credentials").

- [ ] **Step 3: Render check + commit**

```bash
make lint template
git add docs/
git commit -m "docs: sqlStatements escape hatch reference + user guide"
```

---

### Task 6: PR

- [ ] **Step 1: Push and open the PR**

```bash
git push -u origin feat/sqlstatements
gh pr create --title "feat: ProxySQLConfig.spec.sqlStatements raw admin-SQL escape hatch" \
  --body "Implements docs/superpowers/specs/2026-07-21-sqlstatements-escape-hatch-design.md: raw admin SQL executed write-to-all after structured config on every sync pass. Idempotent desired-state semantics; first failure aborts remaining statements and surfaces via existing PartialSync/Degraded conditions; statements participate in lastAppliedHash; not drift-tracked; not reversed on deletion. Unit tests (fake executor), fingerprint pin test, e2e scenario."
```

- [ ] **Step 2: Verify CI green** (`ci` workflow: lint, kubeconform, go test, golangci-lint, trivy, e2e on kind).
