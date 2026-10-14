package controller

import (
	"errors"
	"testing"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

func TestSummarizeReplicasUsesCombinedConvergence(t *testing.T) {
	observations := []replicaObservation{
		{status: proxysqlv1alpha1.ProxySQLConfigReplicaStatus{Pod: "proxy-0", Converged: true, ObservedVersion: 7}, shunned: 1},
		{status: proxysqlv1alpha1.ProxySQLConfigReplicaStatus{Pod: "proxy-1", Drifted: true, Reason: "RDSDrift", ObservedVersion: 7}, drifted: true, shunned: 2},
	}
	statuses, synced, drifted, shunned := summarizeReplicas(observations)
	if len(statuses) != 2 || synced != 1 || drifted != 1 || shunned != 3 {
		t.Fatalf("summary = (%+v, %d, %d, %d)", statuses, synced, drifted, shunned)
	}
}

func TestRunApplyCyclePreservesOrderAndStopsOnFailure(t *testing.T) {
	var order []string
	steps := []applyCycleStep{
		{reason: "RDSClearFailed", run: func() error { order = append(order, "clear-rds"); return nil }},
		{reason: "NativeApplyFailed", run: func() error { order = append(order, "apply-native"); return nil }},
		{reason: "RDSApplyFailed", run: func() error { order = append(order, "apply-rds"); return errors.New("failed") }},
		{reason: "Unexpected", run: func() error { order = append(order, "after-failure"); return nil }},
	}
	if reason := runApplyCycle(steps); reason != "RDSApplyFailed" {
		t.Fatalf("reason = %q", reason)
	}
	want := []string{"clear-rds", "apply-native", "apply-rds"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
