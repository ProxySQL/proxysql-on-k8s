# Milestone 1 — Trustworthy Lifecycle (v0.2.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make ProxySQLConfig lifecycle trustworthy: cleanup on delete (#14), immediate convergence on Secret rotation (#15), runtime status read-back with informed drift resync (#16), admission-time duplicate rejection (#17), and e2e scenarios proving it (#18, operator part).

**Architecture:** All changes live in the existing `ProxySQLConfigReconciler` + `proxysqlclient` package. Cleanup reuses `Sync` with an empty `Desired` (which already DELETEs every table and LOAD/SAVEs — proven by `TestSync_EmptyDesired_StillIssuesDeletesAndLoadSaves`). Read-back adds a `Querier` interface alongside the existing `Executor` so fakes stay trivial. Duplicate rejection uses `listType=map` structural-schema markers (admission-enforced by the API server) instead of CEL — same effect, simpler, still no webhook/cert-manager.

**Tech Stack:** Go 1.25 (`GOTOOLCHAIN=go1.25.10` prefix on the dev machine), controller-runtime, envtest (Ginkgo/Gomega for controller tests, stdlib `testing` for proxysqlclient tests), bash e2e harness in `test/e2e/`.

**Out of scope (separate plan):** the 4 stubbed nightly example flavors (percona-ps, percona-pxc, oracle-mysql-operator, crunchy-pgo) — independent workstream, needs per-backend-operator manifests, no operator code changes.

**Deliberately deferred (decision, not omission):** spec §1.3 also mentions surfacing monitor replication-lag data. That read comes from ProxySQL's `monitor` schema (`mysql_server_replication_lag_log`), whose retention/semantics differ from the `runtime_*` tables and deserve their own small design pass. Defer to a follow-up noted on issue #16 when this plan's PR closes it; `shunnedBackends` (which captures lag-induced shunning via `mysql-monitor_replication_lag` thresholds) ships now.

**Branch:** create `feat/m1-trustworthy-lifecycle` from `main` at execution start (use superpowers:using-git-worktrees). All commits below land there; finish with a PR per repo policy ("Changes must be made through a pull request").

**Conventions that apply to every task** (from CLAUDE.md):
- After editing `operator/api/v1alpha1/*_types.go`: run `make manifests && make sync-crds` from repo root. Never hand-edit `charts/proxysql-operator/crds/`.
- Test command: `cd operator && GOTOOLCHAIN=go1.25.10 make test`. Lint: `cd operator && GOTOOLCHAIN=go1.25.10 make lint`.
- `proxysqlclient.Sync` keeps taking the `Executor` interface; read-back takes the new `Querier` interface. Never a concrete `*Client` inside sync/runtime logic.

---

### Task 1: ProxySQLConfig deletion finalizer (#14)

**Files:**
- Modify: `operator/internal/controller/proxysqlconfig_controller.go`
- Test: `operator/internal/controller/proxysqlconfig_controller_test.go`

Cleanup = push `&proxysqlclient.Desired{}` via the existing `applyToReplicas`: empty Desired DELETEs every managed table and LOAD/SAVEs each section. Variables are intentionally left untouched (empty maps are a no-op in `syncVariables`) — there is no "delete a variable" in ProxySQL, only values, and resetting them blind would be worse.

Wedge policy:
- cluster NotFound → remove finalizer (never wedge on an absent cluster)
- admin Secret NotFound → remove finalizer (cluster effectively torn down)
- cluster exists but 0 ready pods → **requeue** (removing the finalizer would leak config onto pods that come back); the `proxysql.com/skip-cleanup` annotation is the documented escape hatch
- some replicas unreachable → requeue and retry

- [ ] **Step 1: Write the failing envtest tests**

Add to the existing `Describe("ProxySQLConfig Controller", ...)` block in `operator/internal/controller/proxysqlconfig_controller_test.go` (follow the existing fixture style for creating the CR; the snippets below assume `k8sClient`, `ctx`, and a reconciler `r` wired like the existing `It` blocks — reuse the existing setup helpers):

```go
It("adds the cleanup finalizer on reconcile", func() {
    // create a ProxySQLConfig with clusterRef to a non-existent cluster
    cfg := &proxysqlv1alpha1.ProxySQLConfig{
        ObjectMeta: metav1.ObjectMeta{Name: "fin-add", Namespace: "default"},
        Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
            ClusterRef: corev1.LocalObjectReference{Name: "no-such-cluster"},
        },
    }
    Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
    _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fin-add", Namespace: "default"}})
    Expect(err).NotTo(HaveOccurred())
    var got proxysqlv1alpha1.ProxySQLConfig
    Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fin-add", Namespace: "default"}, &got)).To(Succeed())
    Expect(got.Finalizers).To(ContainElement("proxysql.com/config-cleanup"))
})

It("removes the finalizer on delete when the cluster is absent", func() {
    cfg := &proxysqlv1alpha1.ProxySQLConfig{
        ObjectMeta: metav1.ObjectMeta{Name: "fin-nocluster", Namespace: "default"},
        Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
            ClusterRef: corev1.LocalObjectReference{Name: "no-such-cluster"},
        },
    }
    Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
    key := types.NamespacedName{Name: "fin-nocluster", Namespace: "default"}
    _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}) // adds finalizer
    Expect(err).NotTo(HaveOccurred())
    Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())
    _, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key}) // finalize path
    Expect(err).NotTo(HaveOccurred())
    var got proxysqlv1alpha1.ProxySQLConfig
    Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &got))).To(BeTrue())
})

It("honors the skip-cleanup annotation", func() {
    cfg := &proxysqlv1alpha1.ProxySQLConfig{
        ObjectMeta: metav1.ObjectMeta{
            Name: "fin-skip", Namespace: "default",
            Annotations: map[string]string{"proxysql.com/skip-cleanup": "true"},
        },
        Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
            ClusterRef: corev1.LocalObjectReference{Name: "no-such-cluster"},
        },
    }
    Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
    key := types.NamespacedName{Name: "fin-skip", Namespace: "default"}
    _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
    Expect(err).NotTo(HaveOccurred())
    Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())
    _, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
    Expect(err).NotTo(HaveOccurred())
    var got proxysqlv1alpha1.ProxySQLConfig
    Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &got))).To(BeTrue())
})

It("does not remove the finalizer while the cluster exists with no ready pods", func() {
    cluster := &proxysqlv1alpha1.ProxySQLCluster{
        ObjectMeta: metav1.ObjectMeta{Name: "fin-cluster", Namespace: "default"},
    }
    Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
    // The cluster's admin Secret must exist for the no-ready-pods branch to be
    // reached (Secret missing => finalizer removed). Mirror the secret name the
    // builders package derives; check builders.New(...).SecretName() and create
    // a Secret with the radmin key so the code path proceeds to pod discovery.
    b := builders.New(cluster, r.Scheme, builders.Passwords{})
    sec := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{Name: b.SecretName(), Namespace: "default"},
        Data:       map[string][]byte{b.SecretKeys().RadminPassword: []byte("pw")},
    }
    Expect(k8sClient.Create(ctx, sec)).To(Succeed())
    cfg := &proxysqlv1alpha1.ProxySQLConfig{
        ObjectMeta: metav1.ObjectMeta{Name: "fin-wait", Namespace: "default"},
        Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
            ClusterRef: corev1.LocalObjectReference{Name: "fin-cluster"},
        },
    }
    Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
    key := types.NamespacedName{Name: "fin-wait", Namespace: "default"}
    _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
    Expect(err).NotTo(HaveOccurred())
    Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())
    res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
    Expect(err).NotTo(HaveOccurred())
    Expect(res.RequeueAfter).To(Equal(5 * time.Second)) // requeueAfterTransient
    var got proxysqlv1alpha1.ProxySQLConfig
    Expect(k8sClient.Get(ctx, key, &got)).To(Succeed()) // still present, finalizer held
    Expect(got.Finalizers).To(ContainElement("proxysql.com/config-cleanup"))
})
```

Add missing imports to the test file as needed: `apierrors "k8s.io/apimachinery/pkg/api/errors"`, `"time"`, `"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 make test`
Expected: FAIL — finalizer never added (`ContainElement("proxysql.com/config-cleanup")` fails) and delete tests find the object still present.

- [ ] **Step 3: Implement the finalizer in the reconciler**

In `operator/internal/controller/proxysqlconfig_controller.go`:

Add to the `const` block:

```go
	// cfgFinalizer guards ProxySQLConfig deletion: the operator clears the
	// managed admin tables on every ready replica before letting the CR go.
	cfgFinalizer = "proxysql.com/config-cleanup"
	// skipCleanupAnnotation ("true") skips the SQL cleanup on deletion. The
	// escape hatch when the cluster is wedged or unreachable forever.
	skipCleanupAnnotation = "proxysql.com/skip-cleanup"
```

Add import: `"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"`.

In `Reconcile`, immediately after the initial `r.Get` succeeds (before "1) Resolve target cluster"):

```go
	if !cfg.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cfg)
	}
	if controllerutil.AddFinalizer(&cfg, cfgFinalizer) {
		if err := r.Update(ctx, &cfg); err != nil {
			return ctrl.Result{}, err
		}
	}
```

Add the two new methods (place after `applyToReplicas`):

```go
// finalize clears the managed admin tables on every ready replica, then
// releases the finalizer. Policy: never wedge deletion on an absent cluster
// or admin Secret; DO hold the finalizer while the cluster exists but has no
// ready pods (releasing it would leak config onto pods that come back) — the
// skip-cleanup annotation is the escape hatch for that case.
func (r *ProxySQLConfigReconciler) finalize(ctx context.Context, cfg *proxysqlv1alpha1.ProxySQLConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(cfg, cfgFinalizer) {
		return ctrl.Result{}, nil
	}
	if cfg.Annotations[skipCleanupAnnotation] == "true" {
		log.Info("skip-cleanup annotation set; releasing finalizer without cleanup")
		return r.releaseFinalizer(ctx, cfg)
	}

	var cluster proxysqlv1alpha1.ProxySQLCluster
	clusterKey := types.NamespacedName{Name: cfg.Spec.ClusterRef.Name, Namespace: cfg.Namespace}
	if err := r.Get(ctx, clusterKey, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return r.releaseFinalizer(ctx, cfg)
		}
		return ctrl.Result{}, err
	}

	b := builders.New(&cluster, r.Scheme, builders.Passwords{})
	var adminSec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: b.SecretName(), Namespace: cluster.Namespace}, &adminSec); err != nil {
		if apierrors.IsNotFound(err) {
			return r.releaseFinalizer(ctx, cfg)
		}
		return ctrl.Result{}, err
	}
	radminPassword := string(adminSec.Data[b.SecretKeys().RadminPassword])

	addrs, err := r.discoverPodAddresses(ctx, &cluster, b.Spec.Protocols.Admin.Port)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(addrs) == 0 {
		log.Info("cleanup pending: cluster exists but has no ready pods; retrying",
			"cluster", cluster.Name, "escapeHatch", skipCleanupAnnotation)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	// An empty Desired DELETEs every managed table and LOAD/SAVEs each
	// section. Variables are left as-is: ProxySQL has no "unset", and
	// resetting values blind would be worse than leaving them.
	cleaned, errs := r.applyToReplicas(ctx, addrs, radminPassword, &proxysqlclient.Desired{})
	if cleaned != len(addrs) {
		log.Info("cleanup incomplete; retrying", "cleaned", cleaned, "total", len(addrs), "errors", joinErrs(errs))
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}
	return r.releaseFinalizer(ctx, cfg)
}

func (r *ProxySQLConfigReconciler) releaseFinalizer(ctx context.Context, cfg *proxysqlv1alpha1.ProxySQLConfig) (ctrl.Result, error) {
	if controllerutil.RemoveFinalizer(cfg, cfgFinalizer) {
		if err := r.Update(ctx, cfg); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 make test`
Expected: PASS (all four new Its + the existing suite).

- [ ] **Step 5: Lint and commit**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 make lint
git add operator/internal/controller/proxysqlconfig_controller.go operator/internal/controller/proxysqlconfig_controller_test.go
git commit -m "feat(operator): clean up pushed config on ProxySQLConfig deletion (#14)"
```

---

### Task 2: Secret watch for immediate rotation convergence (#15)

**Files:**
- Modify: `operator/internal/controller/proxysqlconfig_controller.go`
- Test: `operator/internal/controller/proxysqlconfig_controller_test.go`

RBAC already grants `secrets: get;list;watch` — no marker changes needed.

- [ ] **Step 1: Write the failing test for the mapper**

```go
It("maps a referenced password Secret to its ProxySQLConfigs", func() {
    cfg := &proxysqlv1alpha1.ProxySQLConfig{
        ObjectMeta: metav1.ObjectMeta{Name: "secmap-cfg", Namespace: "default"},
        Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
            ClusterRef: corev1.LocalObjectReference{Name: "secmap-cluster"},
            MySQLUsers: []proxysqlv1alpha1.MySQLUser{{
                Username: "app",
                PasswordSecretRef: corev1.SecretKeySelector{
                    LocalObjectReference: corev1.LocalObjectReference{Name: "app-user-pw"},
                    Key:                  "password",
                },
            }},
        },
    }
    Expect(k8sClient.Create(ctx, cfg)).To(Succeed())

    reqs := r.configsForSecret(ctx, &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{Name: "app-user-pw", Namespace: "default"},
    })
    Expect(reqs).To(ContainElement(reconcile.Request{
        NamespacedName: types.NamespacedName{Name: "secmap-cfg", Namespace: "default"},
    }))

    // An unrelated secret maps to nothing.
    reqs = r.configsForSecret(ctx, &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
    })
    for _, req := range reqs {
        Expect(req.Name).NotTo(Equal("secmap-cfg"))
    }
})

It("maps a cluster admin Secret to configs targeting that cluster", func() {
    cluster := &proxysqlv1alpha1.ProxySQLCluster{
        ObjectMeta: metav1.ObjectMeta{Name: "secmap-cluster", Namespace: "default"},
    }
    Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
    b := builders.New(cluster, r.Scheme, builders.Passwords{})
    reqs := r.configsForSecret(ctx, &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{Name: b.SecretName(), Namespace: "default"},
    })
    Expect(reqs).To(ContainElement(reconcile.Request{
        NamespacedName: types.NamespacedName{Name: "secmap-cfg", Namespace: "default"},
    }))
})
```

Note: the second It reuses `secmap-cfg` created in the first; if the suite runs Its in random order, create the config in a `BeforeEach`/`JustBeforeEach` or duplicate the create with a distinct name. Follow whatever ordering convention the existing suite uses.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 make test`
Expected: FAIL — compile error, `r.configsForSecret` undefined.

- [ ] **Step 3: Implement the mapper and wire the watch**

In `proxysqlconfig_controller.go`, add after `configsForPod`:

```go
// configsForSecret maps a Secret event to every ProxySQLConfig that consumes
// it — either as a user passwordSecretRef or as the admin Secret of the
// cluster the config targets. This makes password rotation converge on the
// next reconcile instead of waiting for the drift resync interval.
func (r *ProxySQLConfigReconciler) configsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	var configs proxysqlv1alpha1.ProxySQLConfigList
	if err := r.List(ctx, &configs, client.InNamespace(sec.Namespace)); err != nil {
		return nil
	}
	if len(configs.Items) == 0 {
		return nil
	}
	// Clusters whose derived admin-secret name matches this Secret.
	adminOf := map[string]bool{}
	var clusters proxysqlv1alpha1.ProxySQLClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(sec.Namespace)); err == nil {
		for i := range clusters.Items {
			if builders.New(&clusters.Items[i], r.Scheme, builders.Passwords{}).SecretName() == sec.Name {
				adminOf[clusters.Items[i].Name] = true
			}
		}
	}
	var out []reconcile.Request
	for _, c := range configs.Items {
		if adminOf[c.Spec.ClusterRef.Name] || configReferencesSecret(&c, sec.Name) {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name, Namespace: c.Namespace}})
		}
	}
	return out
}

func configReferencesSecret(c *proxysqlv1alpha1.ProxySQLConfig, name string) bool {
	for _, u := range c.Spec.MySQLUsers {
		if u.PasswordSecretRef.Name == name {
			return true
		}
	}
	for _, u := range c.Spec.PostgreSQLUsers {
		if u.PasswordSecretRef.Name == name {
			return true
		}
	}
	return false
}
```

In `SetupWithManager`, add one line after the Pod watch (and extend the doc comment listing the watches):

```go
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.configsForSecret)).
```

A rotated password changes the resolved `Desired`, which changes `syncFingerprint` — so the existing short-circuit naturally lets the push through. No further reconcile changes needed.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 make test`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 make lint
git add operator/internal/controller/proxysqlconfig_controller.go operator/internal/controller/proxysqlconfig_controller_test.go
git commit -m "feat(operator): watch password Secrets so rotation converges immediately (#15)"
```

---

### Task 3: proxysqlclient runtime read-back primitives (#16, client side)

**Files:**
- Create: `operator/internal/proxysqlclient/runtime.go`
- Create: `operator/internal/proxysqlclient/runtime_test.go`
- Modify: `operator/internal/proxysqlclient/client.go` (add `Query`)

- [ ] **Step 1: Write the failing tests**

Create `operator/internal/proxysqlclient/runtime_test.go` (same Apache header as the other files in the package):

```go
package proxysqlclient

import (
	"context"
	"strings"
	"testing"
)

// fakeQuerier returns canned rows keyed by a substring of the query.
type fakeQuerier struct {
	rows map[string][][]string
}

func (f *fakeQuerier) Query(_ context.Context, q string) ([][]string, error) {
	for k, v := range f.rows {
		if strings.Contains(q, k) {
			return v, nil
		}
	}
	return nil, nil
}

// ptr32: if sync_test.go (same package) already defines an equivalent int32
// pointer helper, reuse that one instead — two definitions won't compile.
func ptr32(v int32) *int32 { return &v }

func TestReadRuntime_ParsesTables(t *testing.T) {
	fq := &fakeQuerier{rows: map[string][][]string{
		"runtime_mysql_servers": {
			{"0", "db-0", "3306", "ONLINE"},
			{"1", "db-1", "3306", "SHUNNED"},
		},
		"runtime_mysql_users":       {{"app"}},
		"runtime_mysql_query_rules": {{"1"}},
		"runtime_pgsql_servers":     {},
		"runtime_pgsql_users":       {},
		"runtime_pgsql_query_rules": {},
	}}
	rs, err := ReadRuntime(context.Background(), fq)
	if err != nil {
		t.Fatalf("ReadRuntime: %v", err)
	}
	if got := rs.MySQLServers["0:db-0:3306"]; got != "ONLINE" {
		t.Errorf("server 0:db-0:3306 status = %q, want ONLINE", got)
	}
	if rs.ShunnedCount() != 1 {
		t.Errorf("ShunnedCount = %d, want 1", rs.ShunnedCount())
	}
	if !rs.MySQLUsers["app"] {
		t.Errorf("user app missing from runtime state")
	}
	if !rs.MySQLRules["1"] {
		t.Errorf("rule 1 missing from runtime state")
	}
}

func TestDrift_NoDriftWhenConverged(t *testing.T) {
	d := &Desired{
		MySQLServers: []MySQLServer{{Hostgroup: 0, Hostname: "db-0", Port: 3306}},
		MySQLUsers:   []MySQLUser{{Username: "app", Password: "x"}},
		MySQLQueryRules: []MySQLQueryRule{{RuleID: 1, MatchDigest: "^SELECT", DestinationHostgroup: ptr32(1)}},
	}
	rs := &RuntimeState{
		MySQLServers: map[string]string{"0:db-0:3306": "ONLINE"},
		MySQLUsers:   map[string]bool{"app": true},
		MySQLRules:   map[string]bool{"1": true},
	}
	if diffs := d.Drift(rs); len(diffs) != 0 {
		t.Errorf("Drift = %v, want none", diffs)
	}
}

func TestDrift_DetectsMissingAndExtra(t *testing.T) {
	d := &Desired{
		MySQLServers: []MySQLServer{{Hostgroup: 0, Hostname: "db-0"}}, // Port 0 => defaults to 3306
	}
	rs := &RuntimeState{
		MySQLServers: map[string]string{"0:stale-host:3306": "ONLINE"},
		MySQLUsers:   map[string]bool{"ghost": true},
	}
	diffs := d.Drift(rs)
	want := []string{
		"mysql_servers: extra 0:stale-host:3306",
		"mysql_servers: missing 0:db-0:3306",
		"mysql_users: extra ghost",
	}
	if len(diffs) != len(want) {
		t.Fatalf("Drift = %v, want %v", diffs, want)
	}
	for i := range want {
		if diffs[i] != want[i] {
			t.Errorf("Drift[%d] = %q, want %q", i, diffs[i], want[i])
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 go test ./internal/proxysqlclient/`
Expected: FAIL — compile errors (`ReadRuntime`, `RuntimeState`, `Drift` undefined).

- [ ] **Step 3: Implement `Client.Query` and `runtime.go`**

Append to `operator/internal/proxysqlclient/client.go`:

```go
// Query runs a SELECT and returns all rows as strings. ProxySQL's admin
// interface returns everything as text anyway; string rows keep the caller
// trivial and the fake trivial-er.
func (c *Client) Query(ctx context.Context, query string) ([][]string, error) {
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%s: query %q: %w", c.addr, snippet(query), err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		raw := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rec := make([]string, len(cols))
		for i, v := range raw {
			rec[i] = string(v)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
```

Create `operator/internal/proxysqlclient/runtime.go` (Apache header as elsewhere):

```go
package proxysqlclient

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

// Querier is the subset of *Client that ReadRuntime needs. An interface for
// the same reason Executor is one: tests substitute canned rows.
type Querier interface {
	Query(ctx context.Context, query string) ([][]string, error)
}

// RuntimeState is a key-level snapshot of what one replica is actually
// running, read back from the runtime_* admin tables. Keys, not full rows:
// the operator's question is "did my push land and stick", not "byte-equal".
type RuntimeState struct {
	MySQLServers map[string]string // "hostgroup:hostname:port" -> status (ONLINE/SHUNNED/...)
	MySQLUsers   map[string]bool   // username
	MySQLRules   map[string]bool   // rule_id
	PgSQLServers map[string]string
	PgSQLUsers   map[string]bool
	PgSQLRules   map[string]bool
}

// ReadRuntime queries the runtime_* tables on one admin endpoint.
func ReadRuntime(ctx context.Context, q Querier) (*RuntimeState, error) {
	rs := &RuntimeState{
		MySQLServers: map[string]string{},
		MySQLUsers:   map[string]bool{},
		MySQLRules:   map[string]bool{},
		PgSQLServers: map[string]string{},
		PgSQLUsers:   map[string]bool{},
		PgSQLRules:   map[string]bool{},
	}
	rows, err := q.Query(ctx, "SELECT hostgroup_id, hostname, port, status FROM runtime_mysql_servers")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) >= 4 {
			rs.MySQLServers[r[0]+":"+r[1]+":"+r[2]] = r[3]
		}
	}
	// runtime_mysql_users holds one frontend and one backend row per user;
	// DISTINCT collapses them.
	rows, err = q.Query(ctx, "SELECT DISTINCT username FROM runtime_mysql_users")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) >= 1 {
			rs.MySQLUsers[r[0]] = true
		}
	}
	rows, err = q.Query(ctx, "SELECT rule_id FROM runtime_mysql_query_rules")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) >= 1 {
			rs.MySQLRules[r[0]] = true
		}
	}
	rows, err = q.Query(ctx, "SELECT hostgroup_id, hostname, port, status FROM runtime_pgsql_servers")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) >= 4 {
			rs.PgSQLServers[r[0]+":"+r[1]+":"+r[2]] = r[3]
		}
	}
	rows, err = q.Query(ctx, "SELECT DISTINCT username FROM runtime_pgsql_users")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) >= 1 {
			rs.PgSQLUsers[r[0]] = true
		}
	}
	rows, err = q.Query(ctx, "SELECT rule_id FROM runtime_pgsql_query_rules")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) >= 1 {
			rs.PgSQLRules[r[0]] = true
		}
	}
	return rs, nil
}

// ShunnedCount returns how many backend rows (mysql + pgsql) are SHUNNED.
func (rs *RuntimeState) ShunnedCount() int32 {
	var n int32
	for _, st := range rs.MySQLServers {
		if st == "SHUNNED" {
			n++
		}
	}
	for _, st := range rs.PgSQLServers {
		if st == "SHUNNED" {
			n++
		}
	}
	return n
}

// Drift compares desired state against a replica's runtime snapshot and
// returns a sorted, human-readable list of divergences (empty = converged).
// Comparison is by key (server identity, username, rule id) — attribute-only
// changes (e.g. weight) are carried by the spec-hash path, not by drift
// detection, so they don't need to be re-derived here.
func (d *Desired) Drift(rs *RuntimeState) []string {
	var out []string
	out = append(out, diffKeys("mysql_servers", mysqlServerKeys(d.MySQLServers), keysOfString(rs.MySQLServers))...)
	out = append(out, diffKeys("mysql_users", mysqlUserKeys(d.MySQLUsers), rs.MySQLUsers)...)
	out = append(out, diffKeys("mysql_query_rules", ruleKeys(len(d.MySQLQueryRules), func(i int) int32 { return d.MySQLQueryRules[i].RuleID }), rs.MySQLRules)...)
	out = append(out, diffKeys("pgsql_servers", pgsqlServerKeys(d.PostgreSQLServers), keysOfString(rs.PgSQLServers))...)
	out = append(out, diffKeys("pgsql_users", pgsqlUserKeys(d.PostgreSQLUsers), rs.PgSQLUsers)...)
	out = append(out, diffKeys("pgsql_query_rules", ruleKeys(len(d.PostgreSQLQueryRules), func(i int) int32 { return d.PostgreSQLQueryRules[i].RuleID }), rs.PgSQLRules)...)
	sort.Strings(out)
	return out
}

func diffKeys(table string, want, have map[string]bool) []string {
	var out []string
	for k := range want {
		if !have[k] {
			out = append(out, fmt.Sprintf("%s: missing %s", table, k))
		}
	}
	for k := range have {
		if !want[k] {
			out = append(out, fmt.Sprintf("%s: extra %s", table, k))
		}
	}
	return out
}

func keysOfString(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func mysqlServerKeys(servers []MySQLServer) map[string]bool {
	out := make(map[string]bool, len(servers))
	for _, s := range servers {
		out[fmt.Sprintf("%d:%s:%d", s.Hostgroup, s.Hostname, defaultInt32(s.Port, 3306))] = true
	}
	return out
}

func pgsqlServerKeys(servers []PostgreSQLServer) map[string]bool {
	out := make(map[string]bool, len(servers))
	for _, s := range servers {
		out[fmt.Sprintf("%d:%s:%d", s.Hostgroup, s.Hostname, defaultInt32(s.Port, 5432))] = true
	}
	return out
}

func mysqlUserKeys(users []MySQLUser) map[string]bool {
	out := make(map[string]bool, len(users))
	for _, u := range users {
		out[u.Username] = true
	}
	return out
}

func pgsqlUserKeys(users []PostgreSQLUser) map[string]bool {
	out := make(map[string]bool, len(users))
	for _, u := range users {
		out[u.Username] = true
	}
	return out
}

func ruleKeys(n int, id func(int) int32) map[string]bool {
	out := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		out[strconv.FormatInt(int64(id(i)), 10)] = true
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 go test ./internal/proxysqlclient/`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 make lint
git add operator/internal/proxysqlclient/client.go operator/internal/proxysqlclient/runtime.go operator/internal/proxysqlclient/runtime_test.go
git commit -m "feat(proxysqlclient): runtime read-back (Querier, ReadRuntime, Drift) (#16)"
```

---

### Task 4: Status fields + informed drift resync in the reconciler (#16, controller side)

**Files:**
- Modify: `operator/api/v1alpha1/proxysqlconfig_types.go`
- Modify: `operator/internal/controller/proxysqlconfig_controller.go`
- Modify: `operator/internal/controller/resync_test.go` (if the semantics doc there mentions full re-push, update wording)
- Regenerate: `operator/config/crd/bases/` + `charts/proxysql-operator/crds/` via `make manifests && make sync-crds`

Behavior change: when the drift-resync interval fires and the spec/replica-set/generation are otherwise unchanged, the reconciler now **verifies** runtime state per replica and re-pushes **only drifted replicas**, instead of blind-pushing everything. If read-back fails for a replica, that replica is treated as drifted (we can't prove convergence → re-push). The existing e2e `drift.sh` scenario covers the end-to-end behavior unchanged: out-of-band wipe → detected as drift → re-asserted.

- [ ] **Step 1: Add the status fields**

In `ProxySQLConfigStatus` (after `SyncedReplicas`):

```go
	// DriftedReplicas is the number of ready replicas whose runtime tables
	// diverged from the desired config at the last runtime check. 0 when
	// everything converged.
	// +optional
	DriftedReplicas int32 `json:"driftedReplicas,omitempty"`

	// ShunnedBackends is the total number of backend server rows in SHUNNED
	// state across all replicas at the last runtime check.
	// +optional
	ShunnedBackends int32 `json:"shunnedBackends,omitempty"`

	// LastRuntimeCheckTime is when the operator last read runtime state back
	// from the replicas.
	// +optional
	LastRuntimeCheckTime *metav1.Time `json:"lastRuntimeCheckTime,omitempty"`
```

Update the doc comment on `LastSyncTime` to reflect the new semantics:

```go
	// LastSyncTime is when the operator last asserted desired state on the
	// cluster — either by writing it, or by verifying via runtime read-back
	// that no replica had drifted.
```

Add one printer column to the ProxySQLConfig kubebuilder block (after the "Synced" column):

```go
// +kubebuilder:printcolumn:name="Drifted",type=integer,JSONPath=`.status.driftedReplicas`
```

- [ ] **Step 2: Regenerate CRDs**

Run from repo root: `make manifests && make sync-crds && make kubeconform`
Expected: CRD YAML under `operator/config/crd/bases/` and `charts/proxysql-operator/crds/` both updated; kubeconform passes. Never edit the chart copy by hand.

- [ ] **Step 3: Implement `verifyReplicas` and the informed-resync path**

In `proxysqlconfig_controller.go`, add after `applyToReplicas`:

```go
// verifyReplicas reads runtime state back from each replica and returns the
// addresses whose state drifted from desired, plus the total SHUNNED backend
// count. A replica whose read-back fails is treated as drifted: we cannot
// prove it converged, so it goes back through the push path.
func (r *ProxySQLConfigReconciler) verifyReplicas(ctx context.Context, addrs []string, password string, d *proxysqlclient.Desired) (drifted []string, shunned int32) {
	log := logf.FromContext(ctx)
	for _, addr := range addrs {
		pxc, err := proxysqlclient.New(addr, "radmin", password)
		if err != nil {
			drifted = append(drifted, addr)
			continue
		}
		rs, err := proxysqlclient.ReadRuntime(ctx, pxc)
		_ = pxc.Close()
		if err != nil {
			log.V(1).Info("runtime read-back failed; treating replica as drifted", "addr", addr, "error", err.Error())
			drifted = append(drifted, addr)
			continue
		}
		shunned += rs.ShunnedCount()
		if diffs := d.Drift(rs); len(diffs) > 0 {
			log.Info("runtime drift detected", "addr", addr, "diffs", joinTrunc(diffs, 256))
			drifted = append(drifted, addr)
		}
	}
	return drifted, shunned
}
```

Replace the short-circuit block in `Reconcile` (the section from `hash := syncFingerprint(...)` through the `if allHealthy { ... }` close) with:

```go
	hash := syncFingerprint(desired, addrs)
	unchanged := cfg.Status.LastAppliedHash == hash &&
		cfg.Status.SyncedReplicas == int32(len(addrs)) &&
		cfg.Status.ObservedGeneration == cfg.Generation
	dueForResync := resyncDue(cfg.Status.LastSyncTime, time.Now(), r.resyncInterval())

	if unchanged && !dueForResync {
		log.V(1).Info("ProxySQLConfig unchanged; skipping SQL push", "hash", hash, "replicas", len(addrs))
		return ctrl.Result{RequeueAfter: requeueAfterSuccess}, nil
	}

	pushAddrs := addrs
	if unchanged && dueForResync {
		// Informed resync: nothing about the spec or replica set changed, so
		// instead of blind-pushing everything, read runtime state back and
		// re-push only the replicas that actually drifted.
		drifted, shunned := r.verifyReplicas(ctx, addrs, radminPassword, desired)
		now := metav1.NewTime(time.Now())
		cfg.Status.LastRuntimeCheckTime = &now
		cfg.Status.ShunnedBackends = shunned
		cfg.Status.DriftedReplicas = int32(len(drifted))
		if len(drifted) == 0 {
			// Converged everywhere: verification counts as asserting desired
			// state, so advance the resync clock.
			cfg.Status.LastSyncTime = &now
			if err := r.Status().Update(ctx, &cfg); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueAfterSuccess}, nil
		}
		pushAddrs = drifted
	}

	// 6) Fan out writes.
	synced, syncErrs := r.applyToReplicas(ctx, pushAddrs, radminPassword, desired)
```

And adjust the success/failure accounting right below (the push may have targeted a drifted subset, but success means the whole replica set is converged):

```go
	cfg.Status.ObservedGeneration = cfg.Generation
	if synced == len(pushAddrs) && len(syncErrs) == 0 {
		cfg.Status.SyncedReplicas = int32(len(addrs))
		cfg.Status.DriftedReplicas = 0
		cfg.Status.LastAppliedHash = hash
		now := metav1.NewTime(time.Now())
		cfg.Status.LastSyncTime = &now
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionTrue, "Synced",
			fmt.Sprintf("config applied to %d/%d replicas", len(addrs), len(addrs)))
		r.setCfgCondition(&cfg, cfgCondProgressing, metav1.ConditionFalse, "Steady", "")
		meta.RemoveStatusCondition(&cfg.Status.Conditions, cfgCondDegraded)
		if err := r.Status().Update(ctx, &cfg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfterSuccess}, nil
	}

	// Partial or full failure.
	cfg.Status.SyncedReplicas = int32(len(addrs) - len(pushAddrs) + synced)
	r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "PartialSync",
		fmt.Sprintf("synced %d/%d replicas", synced, len(pushAddrs)))
```

(The rest of the failure path is unchanged.)

- [ ] **Step 4: Run the full operator test suite**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 make test`
Expected: PASS. If `resync_test.go` asserts the old "always full re-push" behavior at the unit level, update its expectations to the new contract: `resyncDue` itself is unchanged; only the reconcile-level behavior on due-resync changed (verify-then-push-drifted). Document the new contract in the test names.

- [ ] **Step 5: Lint, vet, commit**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 make lint
git add operator/api/v1alpha1/proxysqlconfig_types.go operator/api/v1alpha1/zz_generated.deepcopy.go \
        operator/internal/controller/proxysqlconfig_controller.go operator/internal/controller/resync_test.go \
        operator/config/crd/bases/ charts/proxysql-operator/crds/
git commit -m "feat(operator): runtime read-back status + informed drift resync (#16)"
```

---

### Task 5: Admission-time duplicate rejection + pgsql mismatch condition (#17)

**Files:**
- Modify: `operator/api/v1alpha1/proxysqlconfig_types.go` (list markers)
- Modify: `operator/internal/controller/proxysqlconfig_controller.go` (pgsql mismatch condition)
- Test: `operator/internal/controller/proxysqlconfig_controller_test.go`
- Regenerate: CRDs via `make manifests && make sync-crds`

Design note (deviation from the spec's "CEL first" wording, same intent): `listType=map` structural-schema markers give API-server-enforced key uniqueness with zero CEL cost budget concerns and better SSA merge semantics. Still admission-time, still no webhook. Map keys must be required or defaulted — all chosen keys qualify (hostgroup/hostname/username/ruleId are required; port is defaulted).

- [ ] **Step 1: Write the failing envtest test**

envtest installs the real CRDs, so structural-schema validation is enforced:

```go
It("rejects duplicate mysql user names at admission", func() {
    pwRef := corev1.SecretKeySelector{
        LocalObjectReference: corev1.LocalObjectReference{Name: "pw"},
        Key:                  "password",
    }
    cfg := &proxysqlv1alpha1.ProxySQLConfig{
        ObjectMeta: metav1.ObjectMeta{Name: "dup-user", Namespace: "default"},
        Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
            ClusterRef: corev1.LocalObjectReference{Name: "c"},
            MySQLUsers: []proxysqlv1alpha1.MySQLUser{
                {Username: "app", PasswordSecretRef: pwRef},
                {Username: "app", PasswordSecretRef: pwRef},
            },
        },
    }
    Expect(k8sClient.Create(ctx, cfg)).NotTo(Succeed())
})

It("rejects duplicate query rule ids at admission", func() {
    cfg := &proxysqlv1alpha1.ProxySQLConfig{
        ObjectMeta: metav1.ObjectMeta{Name: "dup-rule", Namespace: "default"},
        Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
            ClusterRef: corev1.LocalObjectReference{Name: "c"},
            MySQLQueryRules: []proxysqlv1alpha1.MySQLQueryRule{
                {RuleID: 1}, {RuleID: 1},
            },
        },
    }
    Expect(k8sClient.Create(ctx, cfg)).NotTo(Succeed())
})

It("rejects duplicate server identity at admission", func() {
    cfg := &proxysqlv1alpha1.ProxySQLConfig{
        ObjectMeta: metav1.ObjectMeta{Name: "dup-server", Namespace: "default"},
        Spec: proxysqlv1alpha1.ProxySQLConfigSpec{
            ClusterRef: corev1.LocalObjectReference{Name: "c"},
            MySQLServers: []proxysqlv1alpha1.MySQLServer{
                {Hostgroup: 0, Hostname: "db", Port: 3306},
                {Hostgroup: 0, Hostname: "db", Port: 3306},
            },
        },
    }
    Expect(k8sClient.Create(ctx, cfg)).NotTo(Succeed())
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 make test`
Expected: FAIL — all three Creates currently succeed.

- [ ] **Step 3: Add the list markers**

In `ProxySQLConfigSpec`, annotate each list:

```go
	// MySQL backend topology.
	// +optional
	// +listType=map
	// +listMapKey=hostgroup
	// +listMapKey=hostname
	// +listMapKey=port
	MySQLServers []MySQLServer `json:"mysqlServers,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=username
	MySQLUsers []MySQLUser `json:"mysqlUsers,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=ruleId
	MySQLQueryRules []MySQLQueryRule `json:"mysqlQueryRules,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=writerHostgroup
	MySQLReplicationHostgroups []MySQLReplicationHostgroup `json:"mysqlReplicationHostgroups,omitempty"`

	// PostgreSQL backend topology (ProxySQL 3.x).
	// +optional
	// +listType=map
	// +listMapKey=hostgroup
	// +listMapKey=hostname
	// +listMapKey=port
	PostgreSQLServers []PostgreSQLServer `json:"pgsqlServers,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=username
	PostgreSQLUsers []PostgreSQLUser `json:"pgsqlUsers,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=ruleId
	PostgreSQLQueryRules []PostgreSQLQueryRule `json:"pgsqlQueryRules,omitempty"`

	// ProxySQLServers identifies peer nodes for ProxySQL Cluster sync.
	// When empty, the operator auto-populates from the ProxySQLCluster's StatefulSet pods.
	// +optional
	// +listType=map
	// +listMapKey=hostname
	// +listMapKey=port
	ProxySQLServers []ProxySQLServerEntry `json:"proxysqlServers,omitempty"`
```

Caveat to verify during implementation: `+listMapKey` fields must be required or have a default in the *generated schema*. `port` on `MySQLServer`/`PostgreSQLServer`/`ProxySQLServerEntry` carries `+kubebuilder:default`, `hostgroup`/`hostname`/`username`/`ruleId`/`writerHostgroup` are required (no `omitempty` on required semantics is not the criterion — controller-gen will error out at `make manifests` if a key field doesn't qualify; if it complains about a key, add the missing `+kubebuilder:default` or drop that field from the key set and add a CEL `XValidation` rule for that one list instead).

- [ ] **Step 4: Regenerate CRDs and re-run tests**

Run from repo root: `make manifests && make sync-crds && make kubeconform`
Then: `cd operator && GOTOOLCHAIN=go1.25.10 make test`
Expected: the three new Its PASS. Existing tests that create configs with valid lists are unaffected.

- [ ] **Step 5: Add the pgsql-mismatch Degraded condition**

In `Reconcile`, right after `b := builders.New(...)` / `adminPort` assignment, add:

```go
	// pgsql tables on a cluster that isn't listening on pgsql is almost
	// certainly a user error; we still push (the admin tables exist either
	// way) but surface it loudly.
	pgsqlMismatch := pgsqlConfigured(&cfg) && !b.Spec.Protocols.PostgreSQL.Enabled
```

Add the helper near `isPodReady`:

```go
// pgsqlConfigured reports whether the spec declares any pgsql-side state.
func pgsqlConfigured(cfg *proxysqlv1alpha1.ProxySQLConfig) bool {
	return len(cfg.Spec.PostgreSQLServers)+len(cfg.Spec.PostgreSQLUsers)+len(cfg.Spec.PostgreSQLQueryRules) > 0
}
```

In the success path of `Reconcile`, replace the unconditional `meta.RemoveStatusCondition(&cfg.Status.Conditions, cfgCondDegraded)` with:

```go
		if pgsqlMismatch {
			r.setCfgCondition(&cfg, cfgCondDegraded, metav1.ConditionTrue, "PgsqlDisabled",
				"spec declares pgsql servers/users/rules but the referenced cluster has protocols.pgsql.enabled=false")
		} else {
			meta.RemoveStatusCondition(&cfg.Status.Conditions, cfgCondDegraded)
		}
```

Add an envtest It asserting the condition appears when a config with `pgsqlServers` targets a cluster with pgsql disabled (cluster + admin secret fixtures as in Task 1's last test; the reconcile will stop at NoReadyReplicas, which is fine — assert the condition via a follow-up `Get` after reconcile only if the condition is set before the NoReadyReplicas early return; if it is not, set the condition at the point of computation instead of only in the success path, i.e. call `r.setCfgCondition` immediately when `pgsqlMismatch` is true, before pod discovery).

- [ ] **Step 6: Run tests, lint, commit**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 make test && GOTOOLCHAIN=go1.25.10 make lint
git add operator/api/v1alpha1/ operator/internal/controller/ operator/config/crd/bases/ charts/proxysql-operator/crds/
git commit -m "feat(api): admission-time duplicate rejection via listType=map + pgsql mismatch condition (#17)"
```

---

### Task 6: e2e scenarios — delete cleanup and secret rotation (#18, operator part)

**Files:**
- Create: `test/e2e/scenarios/delete.sh`
- Create: `test/e2e/scenarios/rotate.sh`
- Modify: `test/e2e/run.sh` (SCENARIOS array)

Reuse the helpers from `lib.sh` / existing scenarios: `radmin_pw`, `admin_query`, `wait_config_synced`, `dump_ns`, `fail`, `log`, `$MYSQL_IMAGE`. Match the style of `test/e2e/scenarios/drift.sh`.

- [ ] **Step 1: Write `test/e2e/scenarios/delete.sh`**

```bash
#!/usr/bin/env bash
# Scenario: ProxySQLConfig deletion cleans the pushed config off the proxies.
#  1. Config synced (2 servers + 1 rule visible in runtime tables).
#  2. kubectl delete blocks on the finalizer until cleanup ran.
#  3. Runtime and disk tables are empty afterwards; the pod is still running.

scenario_delete() {
  local ns=e2e-delete
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
  mysqlServers:
    - {hostgroup: 0, hostname: 10.9.9.9, port: 3306}
    - {hostgroup: 1, hostname: 10.9.9.10, port: 3306}
  mysqlQueryRules:
    - {ruleId: 1, matchDigest: "^SELECT", destinationHostgroup: 1, apply: true}
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin out
  radmin="$(radmin_pw "$ns" pxc)"
  out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(*) FROM runtime_mysql_servers")"
  [[ "$out" == "2" ]] || { fail "precondition: expected 2 runtime servers, got '$out'"; dump_ns "$ns"; return 1; }

  # Delete must block until the finalizer cleaned the tables.
  kubectl -n "$ns" delete proxysqlconfig pxcfg --timeout=90s >/dev/null \
    || { fail "delete did not complete within 90s (finalizer wedged?)"; dump_ns "$ns"; return 1; }

  out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(*) FROM runtime_mysql_servers")"
  [[ "$out" == "0" ]] || { fail "runtime_mysql_servers not cleaned (got '$out')"; dump_ns "$ns"; return 1; }
  out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(*) FROM mysql_servers")"
  [[ "$out" == "0" ]] || { fail "mysql_servers (disk) not cleaned (got '$out')"; dump_ns "$ns"; return 1; }
  out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(*) FROM runtime_mysql_query_rules")"
  [[ "$out" == "0" ]] || { fail "runtime_mysql_query_rules not cleaned (got '$out')"; dump_ns "$ns"; return 1; }
  log "delete: config cleaned off the proxy on CR deletion"

  kubectl delete ns "$ns" --wait=false >/dev/null
}
```

- [ ] **Step 2: Write `test/e2e/scenarios/rotate.sh`**

```bash
#!/usr/bin/env bash
# Scenario: rotating a user password Secret re-syncs the config.
#  1. Config with one mysql user synced; record lastAppliedHash.
#  2. Update the Secret -> hash must advance (Secret watch + resolved-password
#     fingerprint) and the user must still be present in runtime.
#  3. status.driftedReplicas stays 0 and lastRuntimeCheckTime eventually sets
#     (read-back ran on a later informed resync).

scenario_rotate() {
  local ns=e2e-rotate
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" create secret generic app-user-pw --from-literal=password=first-pw >/dev/null
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
  mysqlServers:
    - {hostgroup: 0, hostname: 10.9.9.9, port: 3306}
  mysqlUsers:
    - username: app
      passwordSecretRef: {name: app-user-pw, key: password}
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin hash0 hash1 out i
  radmin="$(radmin_pw "$ns" pxc)"
  hash0="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')"

  kubectl -n "$ns" create secret generic app-user-pw --from-literal=password=second-pw \
    --dry-run=client -o yaml | kubectl -n "$ns" apply -f - >/dev/null

  # The Secret watch should re-reconcile promptly; the rotated password is part
  # of the sync fingerprint, so lastAppliedHash must advance.
  for i in $(seq 1 15); do
    hash1="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')"
    [[ -n "$hash1" && "$hash1" != "$hash0" ]] && break
    sleep 4
  done
  [[ "$hash1" != "$hash0" ]] || { fail "lastAppliedHash did not advance after secret rotation"; dump_ns "$ns"; return 1; }

  out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(DISTINCT username) FROM runtime_mysql_users WHERE username='app'")"
  [[ "$out" == "1" ]] || { fail "user 'app' missing from runtime after rotation (got '$out')"; dump_ns "$ns"; return 1; }
  log "rotate: secret rotation propagated (hash advanced, user present)"

  # Read-back fields: after the informed resync interval (the suite shortens
  # it), driftedReplicas must be 0 and lastRuntimeCheckTime must be set.
  for i in $(seq 1 15); do
    [[ -n "$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastRuntimeCheckTime}')" ]] && break
    sleep 4
  done
  out="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastRuntimeCheckTime}')"
  [[ -n "$out" ]] || { fail "lastRuntimeCheckTime never set (read-back did not run)"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.driftedReplicas}')"
  [[ "$out" == "0" || -z "$out" ]] || { fail "driftedReplicas = '$out', want 0"; dump_ns "$ns"; return 1; }
  log "rotate: read-back status populated (drifted=0)"

  kubectl delete ns "$ns" --wait=false >/dev/null
}
```

Note: the suite's shortened resync interval (~15s, set in `lib.sh`/operator install flags) means rotation would converge via resync anyway; the watch-specific immediacy is covered by the Task 2 unit test. This scenario proves the end-to-end contract (rotation converges, status fields populate), not the latency.

- [ ] **Step 3: Register the scenarios**

In `test/e2e/run.sh`, extend the array:

```bash
SCENARIOS=(scenario_mysql scenario_postgres scenario_multireplica scenario_drift scenario_psa scenario_delete scenario_rotate)
```

- [ ] **Step 4: Shellcheck**

Run: `shellcheck -x test/e2e/scenarios/delete.sh test/e2e/scenarios/rotate.sh`
Expected: clean (CI runs shellcheck with external-sources).

- [ ] **Step 5: Run the e2e suite locally**

Run from repo root: `make e2e`
Expected: all 7 scenarios pass, including the existing `scenario_drift` (which now exercises the informed-resync path: out-of-band wipe → read-back detects drift → re-push).
This is the slow step (~10–15 min with kind). If a scenario fails, `dump_ns` output lands in the log — debug before committing.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/scenarios/delete.sh test/e2e/scenarios/rotate.sh test/e2e/run.sh
git commit -m "test(e2e): deletion-cleanup and secret-rotation scenarios (#18)"
```

---

### Task 7: Docs + PR

**Files:**
- Modify: `docs/architecture.md` (finalizer semantics, Secret watch, read-back/informed resync, LastSyncTime semantics change)
- Modify: `README.md` (only if it documents ProxySQLConfig deletion behavior or status fields)

- [ ] **Step 1: Update `docs/architecture.md`**

Add/adjust three passages (match the doc's existing tone):
1. **Deletion**: ProxySQLConfig now carries `proxysql.com/config-cleanup`; deletion clears managed tables on all ready replicas; `proxysql.com/skip-cleanup: "true"` is the escape hatch; variables are never reset (no "unset" in ProxySQL). Wedge policy as in Task 1.
2. **Watches**: add Secrets to the watch list with the rotation rationale.
3. **Drift resync**: replace the "full re-push every interval" description with the informed flow: verify via `runtime_*` read-back → re-push only drifted replicas → `lastSyncTime` now means "last asserted (written or verified)"; new status fields `driftedReplicas`, `shunnedBackends`, `lastRuntimeCheckTime`.

- [ ] **Step 2: Final verification**

```bash
make lint && make template && make kubeconform
cd operator && GOTOOLCHAIN=go1.25.10 make test && GOTOOLCHAIN=go1.25.10 make lint
```
Expected: everything green.

- [ ] **Step 3: Commit docs and open the PR**

```bash
git add docs/architecture.md README.md
git commit -m "docs: lifecycle semantics for v0.2.0 (finalizer, secret watch, informed resync)"
git push -u origin feat/m1-trustworthy-lifecycle
gh pr create --title "Milestone 1: trustworthy lifecycle (v0.2.0)" \
  --body "Implements #14 #15 #16 #17 and the operator part of #18 per docs/superpowers/specs/2026-06-10-operator-roadmap-design.md. Remaining for #18: the four stubbed nightly example flavors (separate PR)."
```

Closes #14, #15, #16, #17; #18 stays open for the nightly example flavors.
