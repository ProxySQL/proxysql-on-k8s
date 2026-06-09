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

package builders

import (
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// PodDisruptionBudget returns the desired PDB, or nil when the PDB is
// disabled or replicas <= 1.
//
// Default policy when no MinAvailable/MaxUnavailable is set:
//
//	replicas = 2 → minAvailable=1
//	replicas ≥ 3 → minAvailable=replicas-1
func (b *Builder) PodDisruptionBudget() *policyv1.PodDisruptionBudget {
	if !isTrue(b.Spec.PodDisruptionBudget.Enabled) {
		return nil
	}
	if b.Spec.Replicas == nil || *b.Spec.Replicas <= 1 {
		return nil
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Name(),
			Namespace: b.Namespace(),
			Labels:    b.Labels(),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: b.SelectorLabels()},
		},
	}
	switch {
	case b.Spec.PodDisruptionBudget.MinAvailable != nil:
		pdb.Spec.MinAvailable = b.Spec.PodDisruptionBudget.MinAvailable
	case b.Spec.PodDisruptionBudget.MaxUnavailable != nil:
		pdb.Spec.MaxUnavailable = b.Spec.PodDisruptionBudget.MaxUnavailable
	default:
		// Sensible default: keep all but one available.
		v := intstr.FromInt32(*b.Spec.Replicas - 1)
		pdb.Spec.MinAvailable = &v
	}
	return pdb
}
