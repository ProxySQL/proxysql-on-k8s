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
