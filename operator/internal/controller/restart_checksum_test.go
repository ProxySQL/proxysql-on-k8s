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

func TestClassifyCnfChange_FreshSTS(t *testing.T) {
	newCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))

	// prev == "" (no StatefulSet yet) must win regardless of oldCnf/newHash.
	verdict, changed := classifyCnfChange("some old cnf", newCnf, "", "new-hash", "")
	if verdict != verdictBootHash {
		t.Errorf("verdict = %v, want verdictBootHash", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty", changed)
	}
}

func TestClassifyCnfChange_UnchangedCnf(t *testing.T) {
	cnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))

	// prev == newHash: already booted on this exact cnf.
	verdict, changed := classifyCnfChange(cnf, cnf, "same-hash", "same-hash", "")
	if verdict != verdictBootHash {
		t.Errorf("verdict = %v, want verdictBootHash", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty", changed)
	}
}

func TestClassifyCnfChange_NoPriorSecret(t *testing.T) {
	newCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))

	// oldCnf == "": no prior Secret to diff against, even though prev is a
	// real (different) hash.
	verdict, changed := classifyCnfChange("", newCnf, "prev-hash", "new-hash", "")
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

	verdict, changed := classifyCnfChange(oldCnf, newCnf, "prev-hash", "new-hash", "")
	if verdict != verdictStructural {
		t.Errorf("verdict = %v, want verdictStructural", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty for a structural verdict", changed)
	}
}

func TestClassifyCnfChange_VariablesOnlyChange(t *testing.T) {
	oldCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "700"))
	newCnf := renderCnf(t, defaultPw, withMySQLVar("mysql-max_connections", "701"))

	verdict, changed := classifyCnfChange(oldCnf, newCnf, "prev-hash", "new-hash", "")
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

	verdict, changed := classifyCnfChange(oldCnf, newCnf, "prev-hash", "new-hash", "")
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
	verdict, changed := classifyCnfChange(cnf, cnf, "prev-hash", "new-hash", appliedVars)
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
	verdict, changed := classifyCnfChange(cnf, cnf, "prev-hash", "new-hash", "stale-or-absent-marker")
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

	verdict, changed := classifyCnfChange(oldCnf, newCnf, "prev-hash", "new-hash", "")
	if verdict != verdictStructural {
		t.Errorf("verdict = %v, want verdictStructural — credential rotation must always force a restart", verdict)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want empty for a structural verdict", changed)
	}
}

func int32Ptr(v int32) *int32 { return &v }
