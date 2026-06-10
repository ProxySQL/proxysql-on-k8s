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

// CnfSecretName returns the name of the Secret carrying the bootstrap
// proxysql.cnf. The "-cnf" suffix avoids colliding with the auth Secret,
// which defaults to the bare cluster name.
func (b *Builder) CnfSecretName() string { return b.Name() + "-cnf" }

// CnfSecret returns the Secret holding the bootstrap proxysql.cnf for the
// StatefulSet pods to mount at /etc/proxysql/proxysql.cnf. It's a Secret —
// not a ConfigMap — because the rendered cnf embeds the admin/radmin/monitor
// passwords. Until v0.3.0 this lived in a ConfigMap named after the cluster;
// the reconciler garbage-collects that leftover on upgrade.
func (b *Builder) CnfSecret() (*corev1.Secret, error) {
	cnf, err := b.BootstrapCnf(b.ProxySQLServerDNS())
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.CnfSecretName(),
			Namespace: b.Namespace(),
			Labels:    b.Labels(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"proxysql.cnf": []byte(cnf),
		},
	}, nil
}
