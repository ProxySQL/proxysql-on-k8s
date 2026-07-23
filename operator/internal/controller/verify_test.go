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
	"context"
	"testing"

	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
)

// Unreachable replicas must be treated as drifted: the operator cannot prove
// they converged, so they must go back through the push path rather than be
// silently counted as healthy.
func TestVerifyReplicasUnreachableIsDrifted(t *testing.T) {
	r := &ProxySQLConfigReconciler{}
	// sql.Open (inside proxysqlclient.New) does not dial — the TCP attempt
	// happens on the first query inside ReadRuntime, which fails with
	// ECONNREFUSED: ports 1 and 2 are privileged and unbound in any sane
	// test environment.
	addrs := []string{"127.0.0.1:1", "127.0.0.1:2"}

	drifted, shunned := r.verifyReplicas(context.Background(), addrs, "pw", &proxysqlclient.Desired{}, nil)

	if len(drifted) != len(addrs) {
		t.Fatalf("drifted=%v, want all of %v", drifted, addrs)
	}
	for i, a := range addrs {
		if drifted[i] != a {
			t.Errorf("drifted[%d]=%q want %q", i, drifted[i], a)
		}
	}
	if shunned != 0 {
		t.Errorf("shunned=%d want 0 (no replica was readable)", shunned)
	}
}
