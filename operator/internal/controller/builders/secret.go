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
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RandomPassword returns a 32-character hex string (~128 bits of entropy).
// Used when the operator has to mint passwords because no existing Secret
// holds them.
func RandomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// AuthSecret builds the desired-state Secret holding admin/radmin/monitor
// passwords. Only used when ManagesAuthSecret() is true.
func (b *Builder) AuthSecret() *corev1.Secret {
	keys := b.SecretKeys()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.SecretName(),
			Namespace: b.Namespace(),
			Labels:    b.Labels(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			keys.AdminPassword:   []byte(b.Pw.Admin),
			keys.RadminPassword:  []byte(b.Pw.Radmin),
			keys.MonitorPassword: []byte(b.Pw.Monitor),
		},
	}
}
