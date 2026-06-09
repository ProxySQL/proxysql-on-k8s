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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConfigMap returns the ConfigMap holding the bootstrap proxysql.cnf for the
// StatefulSet pods to mount at /etc/proxysql/proxysql.cnf.
//
// Note: this ConfigMap contains rendered passwords. That's the same security
// boundary as a Secret in K8s (both are base64 + RBAC-controlled), but worth
// documenting. A future enhancement is to switch to a Secret-backed cnf or
// use an entrypoint substitution pattern; tracked as Phase 6 work.
func (b *Builder) ConfigMap() (*corev1.ConfigMap, error) {
	cnf, err := b.BootstrapCnf(b.ProxySQLServerDNS())
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Name(),
			Namespace: b.Namespace(),
			Labels:    b.Labels(),
		},
		Data: map[string]string{
			"proxysql.cnf": cnf,
		},
	}, nil
}
