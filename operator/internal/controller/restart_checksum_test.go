/*
Copyright 2026 ProxySQL.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
)

// renderCnf renders a bootstrap proxysql.cnf via the real builder pipeline
// (same code path production uses) so these pure-helper tests exercise
// realistic cnf text rather than hand-rolled fixtures.
func renderCnf(t *testing.T, pw builders.Passwords, mut ...func(*proxysqlv1alpha1.ProxySQLCluster)) string {
	t.Helper()
	c := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-test", Namespace: "default"},
	}
	for _, m := range mut {
		m(c)
	}
	b := builders.New(c, nil, pw)
	cnf, err := b.BootstrapCnf(b.ProxySQLServerDNS())
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	return cnf
}

func withMySQLVar(name, value string) func(*proxysqlv1alpha1.ProxySQLCluster) {
	return func(c *proxysqlv1alpha1.ProxySQLCluster) {
		if c.Spec.Variables.MySQL == nil {
			c.Spec.Variables.MySQL = map[string]string{}
		}
		c.Spec.Variables.MySQL[name] = value
	}
}

var defaultPw = builders.Passwords{Admin: "a", Radmin: "r", Monitor: "m"}

// flbKey is the non-proxysql.cnf Secret key used by the extra-key tests.
const flbKey = "fluent-bit.conf"

// secretData wraps a proxysql.cnf string (plus optional extra key/value
// pairs) into the cnf Secret data-map shape classifyCnfChange takes.
func secretData(cnf string, extra ...string) map[string][]byte {
	if len(extra)%2 != 0 {
		panic("secretData: extra must be key/value pairs")
	}
	data := map[string][]byte{"proxysql.cnf": []byte(cnf)}
	for i := 0; i < len(extra); i += 2 {
		data[extra[i]] = []byte(extra[i+1])
	}
	return data
}

func TestClassifyCnfChange_FreshSTS(t *testing.T) {
	newCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))

	// prev == "" (no StatefulSet yet) must win regardless of oldCnf/newHash.
	verdict, changed, keys, _ := classifyCnfChange(secretData("some old cnf"), secretData(newCnf), "", "new-hash", "", "")
	if verdict != verdictBootHash {
		t.Errorf("verdict = %v, want verdictBootHash", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty", changed)
	}
	if len(keys) != 0 {
		t.Errorf("structuralKeys = %v, want empty", keys)
	}
}

func TestClassifyCnfChange_UnchangedCnf(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))

	// prev == newHash: already booted on this exact cnf.
	verdict, changed, _, _ := classifyCnfChange(secretData(cnf), secretData(cnf), "same-hash", "same-hash", "", "")
	if verdict != verdictBootHash {
		t.Errorf("verdict = %v, want verdictBootHash", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty", changed)
	}
}

func TestClassifyCnfChange_NoPriorSecret(t *testing.T) {
	newCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))

	// nil old data: no prior Secret to diff against, even though prev is a
	// real (different) hash.
	verdict, changed, _, _ := classifyCnfChange(nil, secretData(newCnf), "prev-hash", "new-hash", "", "")
	if verdict != verdictBootHash {
		t.Errorf("verdict = %v, want verdictBootHash", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty", changed)
	}
}

func TestClassifyCnfChange_StructuralChange(t *testing.T) {
	oldCnf := renderCnf(t, defaultPw, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Replicas = int32Ptr(1)
	})
	newCnf := renderCnf(t, defaultPw, func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Replicas = int32Ptr(3)
	})

	verdict, changed, keys, _ := classifyCnfChange(secretData(oldCnf), secretData(newCnf), "prev-hash", "new-hash", "", "")
	if verdict != verdictStructural {
		t.Errorf("verdict = %v, want verdictStructural", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty for a structural verdict", changed)
	}
	if len(keys) != 0 {
		t.Errorf("structuralKeys = %v, want empty (proxysql.cnf-internal structural change)", keys)
	}
}

func TestClassifyCnfChange_VariablesOnlyChange(t *testing.T) {
	oldCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	newCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "701"))

	verdict, changed, _, _ := classifyCnfChange(secretData(oldCnf), secretData(newCnf), "prev-hash", "new-hash", "", "")
	if verdict != verdictRuntimeTry {
		t.Fatalf("verdict = %v, want verdictRuntimeTry", verdict)
	}
	if len(changed) != 1 || changed["mysql-max_connections"] != "701" {
		t.Errorf("changed = %v, want {mysql-max_connections: 701}", changed)
	}
}

func TestClassifyCnfChange_RemovedVariableIsStructural(t *testing.T) {
	oldCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	newCnf := renderCnf(t, defaultPw) // variable removed entirely

	verdict, changed, _, _ := classifyCnfChange(secretData(oldCnf), secretData(newCnf), "prev-hash", "new-hash", "", "")
	if verdict != verdictStructural {
		t.Errorf("verdict = %v, want verdictStructural (removed variable changes cnf structure)", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty for a structural verdict", changed)
	}
}

func TestClassifyCnfChange_AlreadyApplied(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	newVars := builders.ParseCnfVariables(cnf)
	appliedVars := varsHash(newVars)

	// oldCnf == newCnf (no diff) and the vars-applied-hash marker already
	// matches: nothing left to do, keep the pod-template annotation as-is.
	verdict, changed, _, _ := classifyCnfChange(secretData(cnf), secretData(cnf), "prev-hash", "new-hash", appliedVars, "")
	if verdict != verdictKeepPrev {
		t.Errorf("verdict = %v, want verdictKeepPrev", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty", changed)
	}
}

func TestClassifyCnfChange_CrashRecoveryPushesFullSet(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"), withMySQLVar("mysql-threads", "4"))
	newVars := builders.ParseCnfVariables(cnf)

	// oldCnf == newCnf (Secret was already updated before the crash) but
	// appliedVars does NOT match: the runtime push never happened or was
	// never confirmed. Must retry with the FULL variable set, not an empty
	// diff.
	verdict, changed, _, _ := classifyCnfChange(secretData(cnf), secretData(cnf), "prev-hash", "new-hash", "stale-or-absent-marker", "")
	if verdict != verdictRuntimeTry {
		t.Fatalf("verdict = %v, want verdictRuntimeTry", verdict)
	}
	if len(changed) != len(newVars) {
		t.Errorf("changed = %v, want the full variable set %v", changed, newVars)
	}
	for k, v := range newVars {
		if changed[k] != v {
			t.Errorf("changed[%q] = %q, want %q", k, changed[k], v)
		}
	}
}

// TestClassifyCnfChange_RevertToBootedValuePushesFullSet pins the C1 fix: a
// pod boots with var=700 (so the pod-template annotation prev equals newHash
// of that cnf), the value is runtime-applied to 701 (Secret now carries 701,
// vars-applied-hash records the 701 set, prev untouched), then the spec
// reverts to 700. Now prev == newHash again, but the LIVE runtime value is
// still 701 — classify must not short-circuit to verdictBootHash (which
// would advance the vars marker and silently drop the 700 push forever); it
// must runtime-push the FULL new variable set, crash-recovery-style.
func TestClassifyCnfChange_RevertToBootedValuePushesFullSet(t *testing.T) {
	bootCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	runtimeCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "701"))
	bootVars := builders.ParseCnfVariables(bootCnf)
	appliedVars := varsHash(builders.ParseCnfVariables(runtimeCnf))
	bootHash := builders.CnfChecksum(secretData(bootCnf))

	// oldData: the Secret as the runtime-apply left it (701). newData: the
	// reverted render (700). prev == newHash == hash of the booted cnf.
	verdict, changed, _, _ := classifyCnfChange(
		secretData(runtimeCnf), secretData(bootCnf),
		bootHash, bootHash, appliedVars, structuralHash(secretData(runtimeCnf)))
	if verdict != verdictRuntimeTry {
		t.Fatalf("verdict = %v, want verdictRuntimeTry — revert to the booted value must still be pushed", verdict)
	}
	if len(changed) != len(bootVars) {
		t.Errorf("changed = %v, want the full new variable set %v", changed, bootVars)
	}
	for k, v := range bootVars {
		if changed[k] != v {
			t.Errorf("changed[%q] = %q, want %q", k, changed[k], v)
		}
	}
}

// TestClassifyCnfChange_UnchangedCnfMatchingMarker: prev == newHash with a
// vars marker that MATCHES the current variable set stays on the bootHash
// path — nothing pending, nothing to push.
func TestClassifyCnfChange_UnchangedCnfMatchingMarker(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	appliedVars := varsHash(builders.ParseCnfVariables(cnf))

	verdict, changed, _, _ := classifyCnfChange(secretData(cnf), secretData(cnf), "same-hash", "same-hash", appliedVars, "")
	if verdict != verdictBootHash {
		t.Errorf("verdict = %v, want verdictBootHash", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty", changed)
	}
}

// TestClassifyCnfChange_CredentialRotationIsAlwaysStructural pins a security
// property flagged in review: rotating the radmin password changes the
// reserved admin_credentials line, which NormalizeCnf leaves verbatim (by
// design — reserved keys are structural, never runtime-appliable). A
// credential rotation must NEVER take the runtime-apply path, because
// runtime-apply only pushes global_variables UPDATEs — it never touches
// admin_credentials, so a runtime-classified credential rotation would
// silently leave the old password live while the annotation claimed the
// change was applied.
func TestClassifyCnfChange_CredentialRotationIsAlwaysStructural(t *testing.T) {
	oldCnf := renderCnf(t, builders.Passwords{Admin: "a", Radmin: "old-radmin-pw", Monitor: "m"})
	newCnf := renderCnf(t, builders.Passwords{Admin: "a", Radmin: "new-radmin-pw", Monitor: "m"})

	verdict, changed, _, _ := classifyCnfChange(secretData(oldCnf), secretData(newCnf), "prev-hash", "new-hash", "", "")
	if verdict != verdictStructural {
		t.Errorf("verdict = %v, want verdictStructural — credential rotation must always force a restart", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty for a structural verdict", changed)
	}
}

// TestClassifyCnfChange_MonitorRotationStaysRuntimeAppliable pins the
// documented monitor-credential exception: mysql-monitor_password is
// reserved against spec.variables override (secret-derived), but when the
// OPERATOR re-renders it from a rotated spec.auth monitor password it is an
// ordinary variable-value change and must keep the restart-free
// runtime-apply path (docs/reference/proxysqlcluster.md, "The
// monitor-credential exception").
func TestClassifyCnfChange_MonitorRotationStaysRuntimeAppliable(t *testing.T) {
	oldCnf := renderCnf(t, builders.Passwords{Admin: "a", Radmin: "r", Monitor: "old-monitor-pw"})
	newCnf := renderCnf(t, builders.Passwords{Admin: "a", Radmin: "r", Monitor: "new-monitor-pw"})

	verdict, changed, _, _ := classifyCnfChange(secretData(oldCnf), secretData(newCnf), "prev-hash", "new-hash", "", "")
	if verdict != verdictRuntimeTry {
		t.Fatalf("verdict = %v, want verdictRuntimeTry — monitor rotation is documented restart-free", verdict)
	}
	if changed["mysql-monitor_password"] != "new-monitor-pw" {
		t.Errorf("changed = %v, want mysql-monitor_password=new-monitor-pw", changed)
	}
}

// TestClassifyCnfChange_ExtraSecretKeyChangeIsStructural pins the regression
// fix: the restart decision guards the WHOLE cnf Secret, not just the
// proxysql.cnf key. A change confined to another key — concretely
// fluent-bit.conf when the logging sink settings change without touching
// proxysql.cnf — must force the restart path, or the fluent-bit sidecar
// keeps running with stale config forever.
func TestClassifyCnfChange_ExtraSecretKeyChangeIsStructural(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))

	oldData := secretData(cnf, flbKey, "[OUTPUT]\n    host old-collector\n")
	newData := secretData(cnf, flbKey, "[OUTPUT]\n    host new-collector\n")

	verdict, changed, keys, _ := classifyCnfChange(oldData, newData, "prev-hash", "new-hash", "", "")
	if verdict != verdictStructural {
		t.Errorf("verdict = %v, want verdictStructural — a non-proxysql.cnf Secret key change must restart", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty for a structural verdict", changed)
	}
	if len(keys) != 1 || keys[0] != flbKey {
		t.Errorf("structuralKeys = %v, want [fluent-bit.conf]", keys)
	}
}

// TestClassifyCnfChange_AddedOrRemovedSecretKeyIsStructural: toggling a key
// into or out of the Secret (fluent-bit.conf appearing/disappearing when
// logging is toggled) is a key-set difference and must restart.
func TestClassifyCnfChange_AddedOrRemovedSecretKeyIsStructural(t *testing.T) {
	cnf := renderCnf(t, defaultPw)

	// Key added.
	verdict, _, keys, _ := classifyCnfChange(secretData(cnf), secretData(cnf, flbKey, "conf"), "prev-hash", "new-hash", "", "")
	if verdict != verdictStructural {
		t.Errorf("added key: verdict = %v, want verdictStructural", verdict)
	}
	if len(keys) != 1 || keys[0] != flbKey {
		t.Errorf("added key: structuralKeys = %v, want [fluent-bit.conf]", keys)
	}

	// Key removed.
	verdict, _, keys, _ = classifyCnfChange(secretData(cnf, flbKey, "conf"), secretData(cnf), "prev-hash", "new-hash", "", "")
	if verdict != verdictStructural {
		t.Errorf("removed key: verdict = %v, want verdictStructural", verdict)
	}
	if len(keys) != 1 || keys[0] != flbKey {
		t.Errorf("removed key: structuralKeys = %v, want [fluent-bit.conf]", keys)
	}
}

// TestClassifyCnfChange_IdenticalExtraKeyKeepsVariablesOnlyVerdict: an extra
// Secret key that is byte-identical on both sides must not disturb the
// variables-only runtime-apply classification of a proxysql.cnf value
// change.
func TestClassifyCnfChange_IdenticalExtraKeyKeepsVariablesOnlyVerdict(t *testing.T) {
	oldCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	newCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "701"))
	const flb = "[OUTPUT]\n    name stdout\n"

	verdict, changed, keys, _ := classifyCnfChange(
		secretData(oldCnf, flbKey, flb),
		secretData(newCnf, flbKey, flb),
		"prev-hash", "new-hash", "", "")
	if verdict != verdictRuntimeTry {
		t.Fatalf("verdict = %v, want verdictRuntimeTry — an identical extra key must not force a restart", verdict)
	}
	if len(changed) != 1 || changed["mysql-max_connections"] != "701" {
		t.Errorf("changed = %v, want {mysql-max_connections: 701}", changed)
	}
	if len(keys) != 0 {
		t.Errorf("structuralKeys = %v, want empty", keys)
	}
}

// TestResolveRestartChecksum_ExtraKeySummaryNamesTheKey: the structural
// verdict caused by a non-proxysql.cnf key must surface that key in the
// Progressing summary. The structural path returns before any pod discovery
// or SQL I/O, so a zero-value reconciler is safe here.
func TestResolveRestartChecksum_ExtraKeySummaryNamesTheKey(t *testing.T) {
	cnf := renderCnf(t, defaultPw)
	oldData := secretData(cnf, flbKey, "old")
	newData := secretData(cnf, flbKey, "new")

	r := &ProxySQLClusterReconciler{}
	cluster := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-test", Namespace: "default"},
	}
	annotation, _, _, summary, err := r.resolveRestartChecksum(
		t.Context(), cluster, oldData, newData, "prev-hash", "new-hash", "", "", "pw")
	if err != nil {
		t.Fatalf("resolveRestartChecksum: %v", err)
	}
	if annotation != "new-hash" {
		t.Errorf("annotation = %q, want %q (structural must roll)", annotation, "new-hash")
	}
	want := "RestartRequired: structural cnf change (fluent-bit.conf)"
	if summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}

// TestStructuralHash_Properties pins the invariant structuralHash is built
// on: variable-VALUE-only proxysql.cnf changes don't move it, while
// structural cnf changes and non-proxysql.cnf key changes do.
func TestStructuralHash_Properties(t *testing.T) {
	cnfA := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	cnfB := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "701"))
	cnfStruct := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"), func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Replicas = int32Ptr(5)
	})

	if structuralHash(secretData(cnfA)) != structuralHash(secretData(cnfB)) {
		t.Errorf("a variable-value-only cnf change must not move structuralHash")
	}
	if structuralHash(secretData(cnfA)) == structuralHash(secretData(cnfStruct)) {
		t.Errorf("a structural cnf change (proxysql_servers) must move structuralHash")
	}
	if structuralHash(secretData(cnfA, flbKey, "x")) == structuralHash(secretData(cnfA, flbKey, "y")) {
		t.Errorf("a non-proxysql.cnf key content change must move structuralHash")
	}
	if structuralHash(secretData(cnfA)) == structuralHash(secretData(cnfA, flbKey, "x")) {
		t.Errorf("adding a non-proxysql.cnf key must move structuralHash")
	}
}

// TestClassifyCnfChange_InterruptedStructuralReconcile simulates the crash
// window the structural-applied marker closes: a prior reconcile wrote the
// new Secret (so oldData==newData now) but died before ensureStatefulSet.
// The vars marker matches (the change wasn't about variables), yet the
// structural marker still reflects the PREVIOUS Secret content — the
// pending restart must not be lost.
func TestClassifyCnfChange_InterruptedStructuralReconcile(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	data := secretData(cnf, flbKey, "new-sink-config")
	appliedVars := varsHash(builders.ParseCnfVariables(cnf))
	staleStructural := structuralHash(secretData(cnf, flbKey, "old-sink-config"))

	verdict, changed, keys, pending := classifyCnfChange(data, data, "prev-hash", "new-hash", appliedVars, staleStructural)
	if verdict != verdictStructural {
		t.Errorf("verdict = %v, want verdictStructural — interrupted structural reconcile must still restart", verdict)
	}
	if !pending {
		t.Errorf("pendingStructural = false, want true")
	}
	if len(changed) != 0 || len(keys) != 0 {
		t.Errorf("changed = %v, structuralKeys = %v, want both empty for the pending case", changed, keys)
	}
}

// TestClassifyCnfChange_MatchingStructuralMarkerKeepsPrev: the happy path is
// unchanged — when the structural marker matches the current Secret data,
// an all-equal reconcile still keeps the previous annotation.
func TestClassifyCnfChange_MatchingStructuralMarkerKeepsPrev(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	data := secretData(cnf, flbKey, "sink-config")
	appliedVars := varsHash(builders.ParseCnfVariables(cnf))

	verdict, _, _, pending := classifyCnfChange(data, data, "prev-hash", "new-hash", appliedVars, structuralHash(data))
	if verdict != verdictKeepPrev {
		t.Errorf("verdict = %v, want verdictKeepPrev", verdict)
	}
	if pending {
		t.Errorf("pendingStructural = true, want false")
	}
}

// TestClassifyCnfChange_FreshSTSIgnoresStructuralMarker: a fresh StatefulSet
// (prev == "") keeps the bootHash behavior no matter what marker value is
// passed — the guards run before the marker check.
func TestClassifyCnfChange_FreshSTSIgnoresStructuralMarker(t *testing.T) {
	cnf := renderCnf(t, defaultPw)
	data := secretData(cnf)

	verdict, _, _, pending := classifyCnfChange(data, data, "", "new-hash", "", "utterly-stale")
	if verdict != verdictBootHash {
		t.Errorf("verdict = %v, want verdictBootHash", verdict)
	}
	if pending {
		t.Errorf("pendingStructural = true, want false")
	}
}

// TestClassifyCnfChange_LegacySTSWithoutStructuralMarker: a StatefulSet from
// an operator version that predates the marker has no annotation
// (structuralApplied == ""). That must NOT force a restart on upgrade — the
// check only fires against a marker that was actually written.
func TestClassifyCnfChange_LegacySTSWithoutStructuralMarker(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	data := secretData(cnf)
	appliedVars := varsHash(builders.ParseCnfVariables(cnf))

	verdict, _, _, pending := classifyCnfChange(data, data, "prev-hash", "new-hash", appliedVars, "")
	if verdict != verdictKeepPrev {
		t.Errorf("verdict = %v, want verdictKeepPrev — absent legacy marker must not force a restart", verdict)
	}
	if pending {
		t.Errorf("pendingStructural = true, want false")
	}
}

// TestClassifyCnfChange_InterruptedCombinedChangeRestarts: crash after a
// Secret write that changed BOTH a variable value and structural content.
// Post-crash both markers are stale; the structural restart must win over
// the vars crash-recovery runtime push — a runtime push would update both
// markers and silently drop the pending restart.
func TestClassifyCnfChange_InterruptedCombinedChangeRestarts(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "701"))
	data := secretData(cnf, flbKey, "new-sink-config")
	oldCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	staleVars := varsHash(builders.ParseCnfVariables(oldCnf))
	staleStructural := structuralHash(secretData(oldCnf, flbKey, "old-sink-config"))

	verdict, _, _, pending := classifyCnfChange(data, data, "prev-hash", "new-hash", staleVars, staleStructural)
	if verdict != verdictStructural {
		t.Errorf("verdict = %v, want verdictStructural — pending structural must win over vars crash-recovery", verdict)
	}
	if !pending {
		t.Errorf("pendingStructural = false, want true")
	}
}

// TestResolveRestartChecksum_PendingStructuralSummary: the interrupted-
// reconcile restart surfaces its own summary and rolls to newHash. The
// structural path returns before any pod discovery or SQL I/O, so a
// zero-value reconciler is safe here.
func TestResolveRestartChecksum_PendingStructuralSummary(t *testing.T) {
	cnf := renderCnf(t, defaultPw)
	data := secretData(cnf, flbKey, "sink-config")
	appliedVars := varsHash(builders.ParseCnfVariables(cnf))
	staleStructural := structuralHash(secretData(cnf, flbKey, "old-sink-config"))

	r := &ProxySQLClusterReconciler{}
	cluster := &proxysqlv1alpha1.ProxySQLCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-test", Namespace: "default"},
	}
	annotation, _, structuralOut, summary, err := r.resolveRestartChecksum(
		t.Context(), cluster, data, data, "prev-hash", "new-hash", appliedVars, staleStructural, "pw")
	if err != nil {
		t.Fatalf("resolveRestartChecksum: %v", err)
	}
	if annotation != "new-hash" {
		t.Errorf("annotation = %q, want %q (pending structural must roll)", annotation, "new-hash")
	}
	if structuralOut != structuralHash(data) {
		t.Errorf("structuralAppliedHash = %q, want structuralHash of the current data", structuralOut)
	}
	want := "RestartRequired: structural change pending from interrupted reconcile"
	if summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}

func int32Ptr(v int32) *int32 { return &v }
