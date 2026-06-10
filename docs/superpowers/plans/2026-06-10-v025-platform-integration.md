# v0.2.5 Platform Integration (part 1: #25 #26 #27) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make ProxySQLCluster consumable by control-plane platforms: web UI exposure (#26), aggregate `status.phase` + `status.endpoints` + `updatedReplicas` (#25), and `username`/`password`-shaped admin Secret compatibility (#27).

**Architecture:** All three land in the cluster side (types, `builders`, `ProxySQLClusterReconciler`); #27 also touches `ProxySQLConfigReconciler`'s admin-password resolution via a new shared helper `builders.PasswordsFromSecret`. Task order matters: web UI (Task 1) before status endpoints (Task 2) because `endpoints.web` depends on it.

**Tech Stack:** Go 1.25 (`GOTOOLCHAIN=go1.25.10`), Kubebuilder/controller-runtime, envtest (Ginkgo), bash e2e in `test/e2e/`.

**Conventions (CLAUDE.md):** pure builders (no I/O); after editing `operator/api/v1alpha1/*_types.go` run `make manifests && make sync-crds` from repo root and commit BOTH CRD copies; never hand-edit `charts/proxysql-operator/crds/`; PSA `restricted` everywhere; tests: `cd operator && GOTOOLCHAIN=go1.25.10 make test`, lint: `... make lint`.

**Branch:** worktree branch `feat/v0.2.5-platform-integration` off main. Finish with a PR (repo requires PRs).

---

### Task 1: Web UI exposure — `protocols.web` (#26)

**Files:**
- Modify: `operator/api/v1alpha1/proxysqlcluster_types.go` (ProtocolsSpec)
- Modify: `operator/internal/controller/builders/builders.go` (const + defaulting)
- Modify: `operator/internal/controller/builders/proxysql_cnf.go` (admin_variables)
- Modify: `operator/internal/controller/builders/service.go` (service port)
- Modify: `operator/internal/controller/builders/statefulset.go` (container port — read the file first and mirror how mysql/pgsql/admin/metrics ports are declared)
- Test: `operator/internal/controller/builders/builders_test.go`
- Regenerate: CRDs via `make manifests && make sync-crds`

ProxySQL's built-in web stats UI is controlled by admin variables `web_enabled` / `web_port` (default 6080). It is HTTPS served by ProxySQL itself.

- [ ] **Step 1: Write failing builder tests**

Add to `operator/internal/controller/builders/builders_test.go`, following its existing table/test style (read it first). Test cases:

```go
func TestDefaultedSpec_WebUI(t *testing.T) {
	// disabled by default
	c := &proxysqlv1alpha1.ProxySQLCluster{}
	spec := DefaultedSpec(c)
	if spec.Protocols.Web.Enabled {
		t.Errorf("web UI must default to disabled")
	}

	// enabled without port => default 6080
	c = &proxysqlv1alpha1.ProxySQLCluster{}
	c.Spec.Protocols.Web.Enabled = true
	spec = DefaultedSpec(c)
	if spec.Protocols.Web.Port != 6080 {
		t.Errorf("web port = %d, want 6080", spec.Protocols.Web.Port)
	}

	// non-zero port implies enabled (same convention as MySQL/PostgreSQL)
	c = &proxysqlv1alpha1.ProxySQLCluster{}
	c.Spec.Protocols.Web.Port = 6081
	spec = DefaultedSpec(c)
	if !spec.Protocols.Web.Enabled || spec.Protocols.Web.Port != 6081 {
		t.Errorf("port-implies-enabled failed: %+v", spec.Protocols.Web)
	}
}

func TestBootstrapCnf_WebUI(t *testing.T) {
	c := &proxysqlv1alpha1.ProxySQLCluster{}
	c.Name = "web-test"
	c.Spec.Protocols.Web.Enabled = true
	b := New(c, scheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"}) // reuse the suite's scheme helper; adapt name if different
	cnf, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	for _, want := range []string{"web_enabled=true", "web_port=6080"} {
		if !strings.Contains(cnf, want) {
			t.Errorf("cnf missing %q:\n%s", want, cnf)
		}
	}
	// and absent when disabled
	c2 := &proxysqlv1alpha1.ProxySQLCluster{}
	c2.Name = "web-off"
	b2 := New(c2, scheme(t), Passwords{})
	cnf2, _ := b2.BootstrapCnf(nil)
	if strings.Contains(cnf2, "web_enabled") {
		t.Errorf("cnf must not mention web_enabled when disabled:\n%s", cnf2)
	}
}

func TestServicePorts_WebUI(t *testing.T) {
	c := &proxysqlv1alpha1.ProxySQLCluster{}
	c.Name = "web-svc"
	c.Spec.Protocols.Web.Enabled = true
	b := New(c, scheme(t), Passwords{})
	svc := b.Service()
	found := false
	for _, p := range svc.Spec.Ports {
		if p.Name == "web" && p.Port == 6080 {
			found = true
		}
	}
	if !found {
		t.Errorf("regular Service missing web port: %+v", svc.Spec.Ports)
	}
	// headless never exposes web (same policy as metrics)
	for _, p := range b.HeadlessService().Spec.Ports {
		if p.Name == "web" {
			t.Errorf("headless Service must not expose web")
		}
	}
}
```

(Adapt the `scheme(t)` helper name and `Passwords` construction to whatever `builders_test.go` actually uses.)

- [ ] **Step 2: Run and verify failure**

Run: `cd operator && GOTOOLCHAIN=go1.25.10 go test ./internal/controller/builders/`
Expected: compile FAIL — `Protocols.Web` undefined.

- [ ] **Step 3: Implement**

`proxysqlcluster_types.go`, in `ProtocolsSpec`:

```go
	// Web exposes ProxySQL's built-in HTTPS stats web UI (admin web_enabled /
	// web_port). Disabled by default; a non-zero port implies enabled.
	// +optional
	Web ProtocolSpec `json:"web,omitempty"`
```

`builders.go`: add `DefaultWebPort int32 = 6080` to the port const block, and in `DefaultedSpec` after the PostgreSQL block:

```go
	// Web UI: disabled by default; enabled only if explicitly toggled or
	// port set.
	if spec.Protocols.Web.Port != 0 {
		spec.Protocols.Web.Enabled = true
	}
	if spec.Protocols.Web.Enabled && spec.Protocols.Web.Port == 0 {
		spec.Protocols.Web.Port = DefaultWebPort
	}
```

`proxysql_cnf.go`: in `admin_variables` (after the restapi block):

```
{{- if .WebEnabled }}
  web_enabled=true
  web_port={{ .WebPort }}
{{- end }}
```

plus `WebEnabled bool` / `WebPort int32` in `cnfData` and wiring in `BootstrapCnf` (`b.Spec.Protocols.Web.Enabled` / `.Port`).

`service.go` in `servicePorts`, after the metrics block (same `!headless` guard — platform/browser traffic, peers don't need it):

```go
	if !headless && b.Spec.Protocols.Web.Enabled {
		ports = append(ports, corev1.ServicePort{
			Name:       "web",
			Port:       b.Spec.Protocols.Web.Port,
			TargetPort: intstr.FromString("web"),
			Protocol:   corev1.ProtocolTCP,
		})
	}
```

`statefulset.go`: add the named container port `web` when `b.Spec.Protocols.Web.Enabled`, mirroring exactly how the existing `metrics` container port is conditionally declared (read the file; do not restructure).

- [ ] **Step 4: Regenerate CRDs, run tests**

From repo root: `make manifests && make sync-crds && make kubeconform` (if helm/kubeconform missing on PATH: `~/go/bin` has kubeconform and helm from previous sessions — `export PATH="$HOME/go/bin:$PATH"`).
Then: `cd operator && GOTOOLCHAIN=go1.25.10 make test`
Expected: PASS.

- [ ] **Step 5: Lint + commit**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 make lint
git add operator/api/v1alpha1/ operator/internal/controller/builders/ operator/config/crd/bases/ charts/proxysql-operator/crds/
git commit -m "feat(api): expose ProxySQL web stats UI via protocols.web (#26)"
```

---

### Task 2: Status phase, endpoints, updatedReplicas (#25)

**Files:**
- Modify: `operator/api/v1alpha1/proxysqlcluster_types.go` (status types + printer column)
- Create: `operator/internal/controller/builders/endpoints.go`
- Modify: `operator/internal/controller/proxysqlcluster_controller.go` (`updateStatus`, degraded path)
- Test: `operator/internal/controller/builders/builders_test.go` (endpoints), `operator/internal/controller/proxysqlcluster_controller_test.go` (phase via envtest)
- Regenerate: CRDs

- [ ] **Step 1: Status types**

In `ProxySQLClusterStatus` (after `ReadyReplicas`):

```go
	// UpdatedReplicas is the number of pods at the current StatefulSet
	// revision.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// Phase is a coarse, single-word projection of the conditions for
	// dashboards and external pollers. Conditions remain the source of truth.
	// One of: Pending, Creating, Running, Updating, Degraded, Failed.
	// (Failed is reserved; the operator currently reports Degraded for error
	// states it can observe.)
	// +optional
	Phase string `json:"phase,omitempty"`

	// Endpoints are the in-cluster DNS endpoints (host:port) for every
	// enabled surface.
	// +optional
	Endpoints *ClusterEndpoints `json:"endpoints,omitempty"`
```

New type next to the status struct:

```go
// ClusterEndpoints lists in-cluster DNS endpoints (host:port) per surface.
// A field is empty when that surface is disabled.
type ClusterEndpoints struct {
	// +optional
	MySQL string `json:"mysql,omitempty"`
	// +optional
	PostgreSQL string `json:"pgsql,omitempty"`
	// +optional
	Admin string `json:"admin,omitempty"`
	// +optional
	Web string `json:"web,omitempty"`
	// +optional
	Metrics string `json:"metrics,omitempty"`
}
```

Phase constants in the same file (typed as plain string consts):

```go
// Phase values for ProxySQLClusterStatus.Phase.
const (
	PhasePending  = "Pending"
	PhaseCreating = "Creating"
	PhaseRunning  = "Running"
	PhaseUpdating = "Updating"
	PhaseDegraded = "Degraded"
	PhaseFailed   = "Failed"
)
```

Printer column after "Ready":

```go
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
```

- [ ] **Step 2: Failing tests**

Builders test (`builders_test.go`):

```go
func TestEndpoints(t *testing.T) {
	c := &proxysqlv1alpha1.ProxySQLCluster{}
	c.Name = "ep"
	c.Namespace = "ns1"
	c.Spec.Protocols.Web.Enabled = true
	b := New(c, scheme(t), Passwords{})
	got := b.Endpoints()
	if got.MySQL != "ep.ns1.svc:6033" { // mysql enabled by default
		t.Errorf("MySQL endpoint = %q", got.MySQL)
	}
	if got.Admin != "ep.ns1.svc:6032" {
		t.Errorf("Admin endpoint = %q", got.Admin)
	}
	if got.Web != "ep.ns1.svc:6080" {
		t.Errorf("Web endpoint = %q", got.Web)
	}
	if got.Metrics != "ep.ns1.svc:6070" { // metrics on by default
		t.Errorf("Metrics endpoint = %q", got.Metrics)
	}
	if got.PostgreSQL != "" { // pgsql off by default
		t.Errorf("PostgreSQL endpoint should be empty, got %q", got.PostgreSQL)
	}
}
```

Envtest spec in `proxysqlcluster_controller_test.go` (follow the existing suite style — read it first; it likely creates a cluster and reconciles). envtest has no kubelet, so pods never become ready: after reconcile the StatefulSet exists with 0 ready replicas → phase must be `Creating`, and `status.endpoints` must be populated:

```go
It("reports phase and endpoints", func() {
	// create cluster (replicas 1), reconcile once (or twice if the suite's
	// pattern needs it for status to settle)
	// then:
	var got proxysqlv1alpha1.ProxySQLCluster
	Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
	Expect(got.Status.Phase).To(Equal(proxysqlv1alpha1.PhaseCreating))
	Expect(got.Status.Endpoints).NotTo(BeNil())
	Expect(got.Status.Endpoints.Admin).To(Equal(got.Name + "." + got.Namespace + ".svc:6032"))
})
```

- [ ] **Step 3: Verify failure** (`make test` → compile fail / assertions fail)

- [ ] **Step 4: Implement**

Create `operator/internal/controller/builders/endpoints.go` (Apache header as elsewhere):

```go
package builders

import (
	"fmt"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// Endpoints returns the in-cluster DNS endpoints for every enabled surface,
// pointing at the regular (load-balanced) Service. Pure projection of the
// defaulted spec; emptiness means "surface disabled".
func (b *Builder) Endpoints() *proxysqlv1alpha1.ClusterEndpoints {
	host := fmt.Sprintf("%s.%s.svc", b.Name(), b.Namespace())
	ep := &proxysqlv1alpha1.ClusterEndpoints{
		Admin: fmt.Sprintf("%s:%d", host, b.Spec.Protocols.Admin.Port),
	}
	if b.Spec.Protocols.MySQL.Enabled {
		ep.MySQL = fmt.Sprintf("%s:%d", host, b.Spec.Protocols.MySQL.Port)
	}
	if b.Spec.Protocols.PostgreSQL.Enabled {
		ep.PostgreSQL = fmt.Sprintf("%s:%d", host, b.Spec.Protocols.PostgreSQL.Port)
	}
	if b.Spec.Protocols.Web.Enabled {
		ep.Web = fmt.Sprintf("%s:%d", host, b.Spec.Protocols.Web.Port)
	}
	if isTrue(b.Spec.Metrics.Enabled) {
		ep.Metrics = fmt.Sprintf("%s:%d", host, b.Spec.Metrics.Port)
	}
	return ep
}
```

In `proxysqlcluster_controller.go` `updateStatus`, after the existing field assignments add:

```go
	cluster.Status.UpdatedReplicas = ss.Status.UpdatedReplicas
	cluster.Status.Endpoints = b.Endpoints()
	cluster.Status.Phase = derivePhase(&ss, apierrors.IsNotFound(err), desired)
```

(note: `err` here is the StatefulSet Get error already in scope — capture `notFound := apierrors.IsNotFound(err)` before the early `return` guard so it can be passed; restructure minimally.)

Add below `updateStatus`:

```go
// derivePhase projects StatefulSet state onto a single coarse phase string.
// Conditions remain the source of truth; this exists for dashboards and
// external pollers. Failed is reserved for future terminal states the
// operator can positively identify.
func derivePhase(ss *appsv1.StatefulSet, ssMissing bool, desired int32) string {
	switch {
	case ssMissing || ss.CreationTimestamp.IsZero():
		return proxysqlv1alpha1.PhasePending
	case ss.Status.ReadyReplicas == 0:
		return proxysqlv1alpha1.PhaseCreating
	case ss.Status.ReadyReplicas == desired &&
		(ss.Status.UpdateRevision == "" || ss.Status.UpdateRevision == ss.Status.CurrentRevision):
		return proxysqlv1alpha1.PhaseRunning
	default:
		return proxysqlv1alpha1.PhaseUpdating
	}
}
```

And in the degraded early-return path in `Reconcile` (the `resolvePasswords` error handler), set the phase before the status write:

```go
		cluster.Status.Phase = proxysqlv1alpha1.PhaseDegraded
		r.setCondition(&cluster, condTypeDegraded, metav1.ConditionTrue, "AuthSecretError", err.Error())
```

- [ ] **Step 5: Regenerate CRDs, full test, lint, commit**

```bash
make manifests && make sync-crds && make kubeconform
cd operator && GOTOOLCHAIN=go1.25.10 make test && GOTOOLCHAIN=go1.25.10 make lint
git add operator/api/v1alpha1/ operator/internal/controller/ operator/config/crd/bases/ charts/proxysql-operator/crds/
git commit -m "feat(operator): status phase, endpoints, updatedReplicas on ProxySQLCluster (#25)"
```

---

### Task 3: username/password-shaped admin Secrets (#27)

**Files:**
- Modify: `operator/internal/controller/builders/builders.go` (Passwords struct + new helper)
- Modify: `operator/internal/controller/builders/proxysql_cnf.go` (extra admin credential)
- Modify: `operator/internal/controller/proxysqlcluster_controller.go` (`resolvePasswords`)
- Modify: `operator/internal/controller/proxysqlconfig_controller.go` (admin password resolution — currently reads `adminSec.Data[keys.RadminPassword]` directly; switch to the shared helper)
- Test: `operator/internal/controller/builders/builders_test.go`, both controller test files

**Design.** Platforms commonly hand over a Secret with `username`/`password` keys. Resolution rules for an **externally managed** Secret (`spec.auth.secretName` set):

1. If the operator-schema keys (admin/radmin/monitor password keys, as configured via `auth.keys`) are present → use them (existing behavior, takes precedence).
2. Else if `username` and `password` keys are present → derive: `Admin = password`, `Radmin = password`, `Monitor = monitor-password` key if present else `password`. If `username` is neither `admin` nor `radmin`, it becomes an **extra remote-capable admin credential** in the bootstrap cnf (`admin_credentials="admin:pw;radmin:pw;<username>:<password>"`) so the platform can connect with its chosen username (ProxySQL only restricts the literal `admin` user to localhost).
3. Else → error (the existing "missing required keys" message, extended to mention both accepted schemas).

Operator-managed Secrets (no `secretName`) are unaffected. The ProxySQLConfig reconciler must resolve radmin through the same rules or rotation/sync breaks for username/password Secrets.

- [ ] **Step 1: Failing unit tests for the shared helper**

```go
func TestPasswordsFromSecret(t *testing.T) {
	keys := proxysqlv1alpha1.AuthKeys{
		AdminPassword: "admin-password", RadminPassword: "radmin-password", MonitorPassword: "monitor-password",
	}

	// operator schema wins even if username/password also present
	pw, err := PasswordsFromSecret(map[string][]byte{
		"admin-password": []byte("a"), "radmin-password": []byte("r"), "monitor-password": []byte("m"),
		"username": []byte("ops"), "password": []byte("x"),
	}, keys)
	if err != nil || pw.Admin != "a" || pw.Radmin != "r" || pw.Monitor != "m" || pw.ExtraAdminUser != "" {
		t.Fatalf("operator schema: pw=%+v err=%v", pw, err)
	}

	// username/password schema
	pw, err = PasswordsFromSecret(map[string][]byte{
		"username": []byte("platform"), "password": []byte("s3cret"),
	}, keys)
	if err != nil || pw.Admin != "s3cret" || pw.Radmin != "s3cret" || pw.Monitor != "s3cret" {
		t.Fatalf("username/password schema: pw=%+v err=%v", pw, err)
	}
	if pw.ExtraAdminUser != "platform" || pw.ExtraAdminPassword != "s3cret" {
		t.Fatalf("extra admin credential not derived: %+v", pw)
	}

	// username == radmin must NOT produce an extra credential
	pw, _ = PasswordsFromSecret(map[string][]byte{
		"username": []byte("radmin"), "password": []byte("s3cret"),
	}, keys)
	if pw.ExtraAdminUser != "" {
		t.Fatalf("radmin username must not duplicate credential: %+v", pw)
	}

	// neither schema -> error naming both
	_, err = PasswordsFromSecret(map[string][]byte{"foo": []byte("bar")}, keys)
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("expected both-schema error, got %v", err)
	}
}

func TestBootstrapCnf_ExtraAdminCredential(t *testing.T) {
	c := &proxysqlv1alpha1.ProxySQLCluster{}
	c.Name = "extra"
	b := New(c, scheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m",
		ExtraAdminUser: "platform", ExtraAdminPassword: "s3cret"})
	cnf, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cnf, `admin_credentials="admin:a;radmin:r;platform:s3cret"`) {
		t.Errorf("extra credential missing:\n%s", cnf)
	}
}
```

- [ ] **Step 2: Verify failure** (compile: `PasswordsFromSecret`, `ExtraAdminUser` undefined)

- [ ] **Step 3: Implement**

`builders.go`:

```go
// Passwords holds the plaintext credentials the operator renders into the
// bootstrap ConfigMap. ExtraAdminUser/ExtraAdminPassword carry an additional
// remote-capable admin credential derived from a username/password-shaped
// external Secret (empty when unused).
type Passwords struct {
	Admin   string
	Radmin  string
	Monitor string

	ExtraAdminUser     string
	ExtraAdminPassword string
}

// PasswordsFromSecret resolves credentials from an auth Secret's data,
// accepting two schemas in precedence order:
//  1. the operator schema (keys per AuthKeys) — all three keys required;
//  2. the common platform schema: "username"/"password" (+ optional
//     monitor key). Admin and radmin share the password; a username other
//     than admin/radmin becomes an extra admin credential in the cnf.
func PasswordsFromSecret(data map[string][]byte, keys proxysqlv1alpha1.AuthKeys) (Passwords, error) {
	admin := string(data[keys.AdminPassword])
	radmin := string(data[keys.RadminPassword])
	monitor := string(data[keys.MonitorPassword])
	if admin != "" && radmin != "" && monitor != "" {
		return Passwords{Admin: admin, Radmin: radmin, Monitor: monitor}, nil
	}

	user := string(data["username"])
	pass := string(data["password"])
	if user != "" && pass != "" {
		pw := Passwords{Admin: pass, Radmin: pass, Monitor: pass}
		if monitor != "" {
			pw.Monitor = monitor
		}
		if user != "admin" && user != "radmin" {
			pw.ExtraAdminUser = user
			pw.ExtraAdminPassword = pass
		}
		return pw, nil
	}

	return Passwords{}, fmt.Errorf(
		"auth secret matches neither schema: need %s/%s/%s, or username/password",
		keys.AdminPassword, keys.RadminPassword, keys.MonitorPassword)
}
```

(add the `fmt` import and the api alias import if missing.)

`proxysql_cnf.go`: change the credentials line to

```
  admin_credentials="admin:{{ .AdminPassword }};radmin:{{ .RadminPassword }}{{ if .ExtraAdminUser }};{{ .ExtraAdminUser }}:{{ .ExtraAdminPassword }}{{ end }}"
```

with `ExtraAdminUser`/`ExtraAdminPassword` added to `cnfData` and wired from `b.Pw`.

`proxysqlcluster_controller.go` `resolvePasswords`: in the externally-managed branch, replace the manual key reads + missing-keys error with:

```go
	if !b.ManagesAuthSecret() {
		return builders.PasswordsFromSecret(sec.Data, keys)
	}
```

keeping the operator-managed backfill branch exactly as-is (it reads the keys individually and mints what's missing — unchanged).

`proxysqlconfig_controller.go`: in `Reconcile`, replace the direct radmin read

```go
	radminPassword := string(adminSec.Data[keys.RadminPassword])
	if radminPassword == "" { ... }
```

with

```go
	adminPw, pwErr := builders.PasswordsFromSecret(adminSec.Data, keys)
	if pwErr != nil {
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "AdminSecretIncomplete", pwErr.Error())
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}
	radminPassword := adminPw.Radmin
```

CAUTION: `finalize` (deletion path) also reads `adminSec.Data[b.SecretKeys().RadminPassword]` directly and releases the finalizer on empty — switch it to `PasswordsFromSecret` too (on error → release finalizer, same "cannot authenticate ⇒ never wedge" policy). Also note the operator-managed path in `resolvePasswords` still requires all three keys present after backfill — `PasswordsFromSecret`'s schema-1 check is consistent with that.

- [ ] **Step 4: envtest coverage**

- Cluster controller spec: external Secret with only `username: platform` / `password: s3cret` → reconcile succeeds (no AuthSecretError), and the generated ConfigMap's `proxysql.cnf` contains `platform:s3cret` in admin_credentials.
- Config controller spec: same external-secret cluster + a ProxySQLConfig → reconcile passes the AdminSecret stage (gets to NoReadyReplicas, not AdminSecretIncomplete).
- Negative: external Secret with neither schema → cluster Degraded/AuthSecretError mentioning both schemas.

- [ ] **Step 5: Full test, lint, commit**

```bash
cd operator && GOTOOLCHAIN=go1.25.10 make test && GOTOOLCHAIN=go1.25.10 make lint
git add operator/internal/ operator/api/ 2>/dev/null; git add operator/config/crd/bases/ charts/proxysql-operator/crds/ 2>/dev/null
git commit -m "feat(operator): accept username/password-shaped admin Secrets (#27)"
```

---

### Task 4: e2e scenario `platform.sh` (#25/#26/#27 end-to-end)

**Files:**
- Create: `test/e2e/scenarios/platform.sh`
- Modify: `test/e2e/run.sh` (SCENARIOS array)

One scenario covering the platform-consumption story (mirror the style of `test/e2e/scenarios/delete.sh`; helpers in `lib.sh`):

```bash
#!/usr/bin/env bash
# Scenario: the "platform integration" surface — a control plane pre-creates a
# username/password admin Secret, enables the web UI, then polls phase and
# endpoints instead of inspecting Services/StatefulSets.

scenario_platform() {
  local ns=e2e-platform
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" create secret generic platform-admin \
    --from-literal=username=platform --from-literal=password=plat-secret >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  auth: {secretName: platform-admin}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: false}, web: {enabled: true}}
YAML
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null

  local out i
  # Phase converges to Running once the pod is ready.
  for i in $(seq 1 15); do
    out="$(kubectl -n "$ns" get proxysqlcluster pxc -o jsonpath='{.status.phase}')"
    [[ "$out" == "Running" ]] && break
    sleep 2
  done
  [[ "$out" == "Running" ]] || { fail "platform: phase='$out', want Running"; dump_ns "$ns"; return 1; }
  log "platform: phase=Running"

  out="$(kubectl -n "$ns" get proxysqlcluster pxc -o jsonpath='{.status.endpoints.mysql}')"
  [[ "$out" == "pxc.${ns}.svc:6033" ]] || { fail "platform: endpoints.mysql='$out'"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get proxysqlcluster pxc -o jsonpath='{.status.endpoints.web}')"
  [[ "$out" == "pxc.${ns}.svc:6080" ]] || { fail "platform: endpoints.web='$out'"; dump_ns "$ns"; return 1; }
  log "platform: endpoints published (mysql, web)"

  # The platform's own username works on the admin port (remote).
  out="$(kubectl -n "$ns" run e2e-admincheck --rm -i --restart=Never --image="$MYSQL_IMAGE" --command -- \
    mysql -h pxc -P6032 -uplatform -pplat-secret -N -e "SELECT 1" 2>/dev/null | tail -1)"
  [[ "$out" == "1" ]] || { fail "platform: admin login with username/password secret failed (got '$out')"; dump_ns "$ns"; return 1; }
  log "platform: username/password admin credential works remotely"

  # Web UI answers on 6080 (HTTPS, self-signed -> -k; any HTTP response counts).
  kubectl -n "$ns" run e2e-webcheck --rm -i --restart=Never --image=curlimages/curl:8.7.1 --command -- \
    curl -ksS -o /dev/null -w '%{http_code}' "https://pxc.${ns}.svc:6080/" | grep -qE '^[0-9]{3}$' \
    || { fail "platform: web UI not answering on 6080"; dump_ns "$ns"; return 1; }
  log "platform: web UI answering on 6080"

  kubectl delete ns "$ns" --wait=false >/dev/null
}
```

Register `scenario_platform` in `run.sh`'s SCENARIOS array. Shellcheck via `docker run --rm -v "$PWD/test:/mnt/test:ro" koalaman/shellcheck:stable -x /mnt/test/e2e/scenarios/platform.sh`. Note: verify the web-check command shape works (`kubectl run ... | grep` exit codes) — if brittle, capture output into a variable first like the admin check does.

Then the full suite from repo root: `export PATH="$HOME/go/bin:$PATH" && make e2e` — all 8 scenarios must pass.

Commit:
```bash
git add test/e2e/scenarios/platform.sh test/e2e/run.sh
git commit -m "test(e2e): platform-integration scenario (phase, endpoints, web UI, username/password secret) (#25 #26 #27)"
```

---

### Task 5: Docs + PR

- [ ] Update `docs/architecture.md`: ProxySQLCluster status section (phase projection + endpoints + updatedReplicas), protocols.web, the two accepted admin-Secret schemas with precedence (operator schema wins) and the extra-admin-credential behavior.
- [ ] Final verification: `make lint && make template && make kubeconform`; `cd operator && GOTOOLCHAIN=go1.25.10 make test && GOTOOLCHAIN=go1.25.10 make lint`.
- [ ] Commit docs, push branch, open PR:

```bash
git add docs/architecture.md
git commit -m "docs: platform integration surface (phase/endpoints, web UI, admin secret schemas)"
git push -u origin feat/v0.2.5-platform-integration
gh pr create --title "v0.2.5 part 1: platform integration surface (#25 #26 #27)" \
  --body "status.phase/endpoints/updatedReplicas (#25), protocols.web (#26), username/password admin Secrets (#27) per docs/superpowers/specs/2026-06-10-operator-roadmap-design.md §1.5.1-1.5.3. Full kind e2e (8 scenarios) green. Closes #25, closes #26, closes #27."
```

---

## Self-review notes

- Task order is load-bearing: Task 2's `Endpoints()` references `Protocols.Web` from Task 1.
- `derivePhase` deliberately maps "SS exists, 0 ready" to Creating even during a total outage of a previously-running cluster; acceptable coarse semantics for v1 of the projection (conditions carry the detail). Documented in the function comment.
- #27 keeps the operator-managed Secret path byte-for-byte unchanged; only externally-managed resolution gains the second schema, and the config reconciler + finalizer move to the shared helper so all three resolution sites agree.
