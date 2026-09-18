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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCheckTopologyVersion(t *testing.T) {
	cases := []struct {
		tag     string
		refused bool
	}{
		// Refused: positively parses as older than the x.y.8 floor.
		{"3.0.7", true},
		{"v3.0.0", true},
		{"3.1.7", true},
		{"4.0.7-debian", true},
		// Allowed: at or above the floor.
		{"3.0.8", false},
		{"3.0.11", false},
		{"4.0.9", false},
		{"3.1.8-alpine", false},
		// Allowed because unparseable — a mirror, a digest pin, a
		// series tag or a vendor scheme the operator has no catalog for.
		// The SaaS validates versions up front where it does.
		{"", false},
		{"3.0", false},
		{"latest", false},
		{"sha256:deadbeef", false},
		{"2026.04.1", false}, // parses, but patch 1 of a calendar scheme...
		{"main-7f3c1a2", false},
		{"custom.build.x", false},
		{"3.x.7", false},
	}
	for _, c := range cases {
		err := checkTopologyVersion(c.tag)
		if c.refused && err == nil {
			t.Errorf("checkTopologyVersion(%q) = nil, want refusal", c.tag)
		}
		if !c.refused && err != nil {
			t.Errorf("checkTopologyVersion(%q) = %v, want allowed", c.tag, err)
		}
	}
}

// TestPVCBelongsToStatefulSet pins the name rule the PVC prune relies on:
// only "<claimTemplate>-<set>-<ordinal>" belongs to a set. A label rule
// cannot do this — a StatefulSet's PVCs inherit its selector labels, and the
// single-set selector is a subset of every role selector.
func TestPVCBelongsToStatefulSet(t *testing.T) {
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pxc-core-us-east-1a"},
		Spec: appsv1.StatefulSetSpec{
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "data"}},
			},
		},
	}
	cases := []struct {
		pvc  string
		want bool
	}{
		{"data-pxc-core-us-east-1a-0", true},
		{"data-pxc-core-us-east-1a-11", true},
		{"data-pxc-core-us-east-1b-0", false},  // another zone's set
		{"data-pxc-satellite-0", false},        // another role's set
		{"data-pxc-0", false},                  // the bare set
		{"logs-pxc-core-us-east-1a-0", false},  // not a claim template of ours
		{"data-pxc-core-us-east-1a-", false},   // no ordinal
		{"data-pxc-core-us-east-1a-x", false},  // not an ordinal
		{"data-pxc-core-us-east-1a-0x", false}, // not an ordinal
	}
	for _, c := range cases {
		if got := pvcBelongsToStatefulSet(c.pvc, ss); got != c.want {
			t.Errorf("pvcBelongsToStatefulSet(%q) = %v, want %v", c.pvc, got, c.want)
		}
	}
}
