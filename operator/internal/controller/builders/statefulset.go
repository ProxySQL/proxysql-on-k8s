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
	"maps"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// StatefulSet builds the desired-state StatefulSet. cnfChecksum is the
// deterministic SHA over every cnf Secret key (proxysql.cnf, plus
// fluent-bit.conf when logging is enabled); included as a pod-template
// annotation so config changes trigger a rolling restart.
func (b *Builder) StatefulSet(cnfChecksum string) *appsv1.StatefulSet {
	labels := b.Labels()
	selector := b.SelectorLabels()

	podLabels := make(map[string]string, len(selector)+len(b.Spec.PodLabels))
	maps.Copy(podLabels, selector)
	maps.Copy(podLabels, b.Spec.PodLabels)

	// User annotations first, reserved keys last: proxysql.com/cnf-checksum
	// (and the TLS rotation-fallback bump) are rollout triggers, so a
	// user-supplied podAnnotations entry with the same key must never
	// clobber them.
	podAnnotations := make(map[string]string, len(b.Spec.PodAnnotations)+2)
	maps.Copy(podAnnotations, b.Spec.PodAnnotations)
	podAnnotations["proxysql.com/cnf-checksum"] = cnfChecksum
	if b.Spec.TLSEnabled() && b.TLSRestartValue != "" {
		podAnnotations[TLSRestartAnnotation] = b.TLSRestartValue
	}

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Name(),
			Namespace: b.Namespace(),
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         b.HeadlessName(),
			Replicas:            b.effectiveReplicas(),
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector: &metav1.LabelSelector{
				MatchLabels: selector,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: b.podSpec(),
			},
		},
	}

	if isTrue(b.Spec.Persistence.Enabled) {
		ss.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{b.dataPVC()}
	}

	return ss
}

// effectiveReplicas returns the replica count the StatefulSet should carry:
// 0 when the cluster is paused (spec.pause), otherwise the defaulted
// spec.Replicas unchanged. Pausing never mutates spec.Replicas itself —
// only the StatefulSet's actual replica count — so resuming (pause=false)
// restores the StatefulSet to spec.Replicas with no further input needed.
func (b *Builder) effectiveReplicas() *int32 {
	if b.Spec.Pause {
		zero := int32(0)
		return &zero
	}
	return b.Spec.Replicas
}

func (b *Builder) podSpec() corev1.PodSpec {
	podSecurityContext := b.Spec.PodSecurityContext
	if sysctls := b.keepaliveSysctls(); len(sysctls) > 0 {
		// Copy before appending: the defaulted spec's security context is
		// shared and builders must stay side-effect free.
		podSecurityContext = podSecurityContext.DeepCopy()
		podSecurityContext.Sysctls = append(podSecurityContext.Sysctls, sysctls...)
	}

	spec := corev1.PodSpec{
		ImagePullSecrets:              b.Spec.ImagePullSecrets,
		SecurityContext:               podSecurityContext,
		NodeSelector:                  b.Spec.NodeSelector,
		Tolerations:                   b.Spec.Tolerations,
		Affinity:                      b.Spec.Affinity,
		TerminationGracePeriodSeconds: ptrInt64(30),
		Containers:                    []corev1.Container{b.container()},
		Volumes: []corev1.Volume{
			{
				Name: "config",
				VolumeSource: corev1.VolumeSource{
					// Secret, not ConfigMap: the rendered cnf embeds passwords.
					Secret: &corev1.SecretVolumeSource{
						SecretName: b.CnfSecretName(),
						Items: []corev1.KeyToPath{
							{Key: "proxysql.cnf", Path: "proxysql.cnf"},
						},
					},
				},
			},
		},
	}

	// When persistence is disabled, mount an emptyDir for /var/lib/proxysql so
	// the readOnlyRootFilesystem container has somewhere to write its admin DB.
	if !isTrue(b.Spec.Persistence.Enabled) {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	// Optional Fluent Bit log-shipping sidecar (spec.logging): a regular
	// container plus the shared logs emptyDir and its config projection.
	if b.LoggingEnabled() {
		spec.Containers = append(spec.Containers, b.fluentBitContainer())
		spec.Volumes = append(spec.Volumes, b.loggingVolumes()...)
	}

	// TLS (spec.tls): the serving-cert Secret volume plus the init
	// container that symlinks the fixed datadir cert names into it — see
	// tlsInitContainer for why symlinks and not variables.
	if b.Spec.TLSEnabled() {
		spec.InitContainers = append(spec.InitContainers, b.tlsInitContainer())
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: b.tlsMountSecretName(),
					Items: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "tls.crt"},
						{Key: "tls.key", Path: "tls.key"},
						{Key: "ca.crt", Path: "ca.crt"},
					},
				},
			},
		})
		if b.backendTLSEnabled() {
			spec.Volumes = append(spec.Volumes, b.backendTLSVolume())
		}
	} else if b.TLSCleanup && isTrue(b.Spec.Persistence.Enabled) {
		// TLS was disabled on a previously-wired cluster with a persistent
		// datadir: remove the (now dangling) cert symlinks before proxysql
		// starts, or it exits at boot — see TLSCleanup's field comment.
		spec.InitContainers = append(spec.InitContainers, b.tlsCleanupInitContainer())
	}

	return spec
}

// TLS volume/mount layout. The serving-cert Secret projects at
// tlsMountPath; the backend trust material (a DIFFERENT PKI — the
// database's issuer) projects at backendTLSMountPath, matching the
// ssl_p2s_* paths rendered into the cnf (tlsCnfVars). Both are nested
// under the /etc/proxysql config mount — kubelet mounts nested paths in
// order, so a mount point inside another read-only mount is fine.
const (
	tlsVolumeName        = "tls"
	tlsMountPath         = "/etc/proxysql/tls"
	backendTLSVolumeName = "backend-tls"
	backendTLSMountPath  = "/etc/proxysql/backend-tls"
)

// Exported TLS pod-template markers. The reconciler's validate-and-hold
// logic (tls_secrets.go) inspects the EXISTING StatefulSet for these to
// decide whether TLS was previously wired — and, when a re-resolution
// fails, which Secret the last-good template mounted.
const (
	// TLSVolumeName is the name of the serving-cert Secret volume.
	TLSVolumeName = tlsVolumeName
	// BackendTLSVolumeName is the name of the backend trust projected volume.
	BackendTLSVolumeName = backendTLSVolumeName
	// TLSInitContainerName is the name of the datadir-symlink init container.
	TLSInitContainerName = "tls-init"
	// TLSCleanupInitContainerName is the name of the disable-transition
	// init container that removes the datadir cert symlinks (see
	// Builder.TLSCleanup).
	TLSCleanupInitContainerName = "tls-cleanup"
	// TLSRestartAnnotation is the pod-template annotation the TLS rotation
	// engine bumps (to the tls Secret's content hash) when a replica fails
	// handshake verification after PROXYSQL RELOAD TLS: rotation changes
	// Secret CONTENT, never cnf text, so the cnf checksum cannot carry
	// this rollout (see Builder.TLSRestartValue).
	TLSRestartAnnotation = "proxysql.com/tls-restart"
)

// tlsInitContainer seeds the datadir cert symlinks before proxysql starts.
//
// ProxySQL 3.0 has no frontend/admin cert-path variables (probe-verified;
// see tlsCnfVars in proxysql_cnf.go): it loads — or, when absent,
// auto-generates — the fixed datadir files proxysql-{ca,cert,key}.pem, and
// `PROXYSQL RELOAD TLS` re-reads exactly those paths. Symlinking them into
// the read-only Secret mount gives cert delivery AND restart-free
// rotation: kubelet updates the mounted Secret atomically, the symlinks
// keep resolving, RELOAD TLS picks up new content. `ln -sfn` is idempotent
// and replaces real pem files left by a pre-TLS boot on a persistent
// datadir (all three flows boot-probe verified on proxysql/proxysql:3.0:
// fresh+symlinked, admin handshake, and persistent-datadir reseed — see
// .superpowers/sdd/task-3-report.md).
//
// The container reuses the cluster's own proxysql image (no extra pull,
// same digest pinning) — any image with /bin/sh works — and the main
// container's securityContext, so PSA `restricted` compliance is identical.
func (b *Builder) tlsInitContainer() corev1.Container {
	return corev1.Container{
		Name:            TLSInitContainerName,
		Image:           b.Image(),
		ImagePullPolicy: b.Spec.Image.PullPolicy,
		SecurityContext: b.Spec.ContainerSecurityContext,
		Command:         []string{"sh", "-c"},
		Args: []string{
			"ln -sfn " + tlsMountPath + "/tls.crt /var/lib/proxysql/proxysql-cert.pem && " +
				"ln -sfn " + tlsMountPath + "/tls.key /var/lib/proxysql/proxysql-key.pem && " +
				"ln -sfn " + tlsMountPath + "/ca.crt /var/lib/proxysql/proxysql-ca.pem",
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true},
			{Name: "data", MountPath: "/var/lib/proxysql"},
		},
	}
}

// tlsCleanupInitContainer removes the datadir cert symlinks after TLS is
// disabled on a previously-wired cluster with a persistent datadir.
//
// Probe-verified on proxysql/proxysql:3.0 (image 77bfbfc3d21c; evidence in
// .superpowers/sdd/task-5-report.md): with the tls Secret mount gone, the
// retained proxysql-{ca,cert,key}.pem symlinks dangle; ProxySQL reads them
// as "no SSL keys/certificates found", tries to auto-generate THROUGH the
// dangling symlink, fails on BIO_new_file (the target directory no longer
// exists) and the process EXITS — the pod would crash-loop forever. The
// `[ -L ... ] && rm` guard removes symlinks ONLY: real pem files (ProxySQL's
// own autogen on a datadir that never had TLS) are untouched, so running
// this container is idempotent and safe on every boot.
func (b *Builder) tlsCleanupInitContainer() corev1.Container {
	return corev1.Container{
		Name:            TLSCleanupInitContainerName,
		Image:           b.Image(),
		ImagePullPolicy: b.Spec.Image.PullPolicy,
		SecurityContext: b.Spec.ContainerSecurityContext,
		Command:         []string{"sh", "-c"},
		Args: []string{
			`for f in proxysql-ca.pem proxysql-cert.pem proxysql-key.pem; do` +
				` [ -L "/var/lib/proxysql/$f" ] && rm -f "/var/lib/proxysql/$f"; done; true`,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/var/lib/proxysql"},
		},
	}
}

// backendTLSVolume projects the backend trust material: the CA bundle from
// spec.tls.backend.caSecretName (ca.crt), plus — when referenced — the
// mTLS client pair from clientCertSecretName (tls.crt/tls.key). One
// projected volume keeps a single mount path for all ssl_p2s_* variables.
func (b *Builder) backendTLSVolume() corev1.Volume {
	sources := []corev1.VolumeProjection{{
		Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: b.Spec.TLS.Backend.CASecretName},
			Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
		},
	}}
	if b.Spec.TLS.Backend.ClientCertSecretName != "" {
		sources = append(sources, corev1.VolumeProjection{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: b.Spec.TLS.Backend.ClientCertSecretName},
				Items: []corev1.KeyToPath{
					{Key: "tls.crt", Path: "tls.crt"},
					{Key: "tls.key", Path: "tls.key"},
				},
			},
		})
	}
	return corev1.Volume{
		Name: backendTLSVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{Sources: sources},
		},
	}
}

func (b *Builder) container() corev1.Container {
	ports := []corev1.ContainerPort{
		{Name: portNameAdmin, ContainerPort: b.Spec.Protocols.Admin.Port, Protocol: corev1.ProtocolTCP},
	}
	if b.Spec.Protocols.MySQL.IsEnabled() {
		ports = append(ports, corev1.ContainerPort{Name: portNameMySQL, ContainerPort: b.Spec.Protocols.MySQL.Port, Protocol: corev1.ProtocolTCP})
	}
	if b.Spec.Protocols.PostgreSQL.IsEnabled() {
		ports = append(ports, corev1.ContainerPort{Name: portNamePgSQL, ContainerPort: b.Spec.Protocols.PostgreSQL.Port, Protocol: corev1.ProtocolTCP})
	}
	if isTrue(b.Spec.Metrics.Enabled) {
		ports = append(ports, corev1.ContainerPort{Name: portNameMetrics, ContainerPort: b.Spec.Metrics.Port, Protocol: corev1.ProtocolTCP})
	}
	if b.Spec.Protocols.Web.IsEnabled() {
		ports = append(ports, corev1.ContainerPort{Name: portNameWeb, ContainerPort: b.Spec.Protocols.Web.Port, Protocol: corev1.ProtocolTCP})
	}

	return corev1.Container{
		Name:            "proxysql",
		Image:           b.Image(),
		ImagePullPolicy: b.Spec.Image.PullPolicy,
		SecurityContext: b.Spec.ContainerSecurityContext,
		// The proxysql/proxysql image has no ENTRYPOINT — the binary name is the
		// first token of its CMD ("proxysql -f --idle-threads -D ..."). Overriding
		// args without command makes Kubernetes exec "-f" directly, and the
		// container CrashLoops with `exec: "-f": executable file not found`. So
		// command must be set explicitly.
		Command: []string{"proxysql"},
		// --reload merges the bootstrap cnf over the persisted proxysql.db on
		// every start (issue #50): without it, an existing proxysql.db on a PVC
		// wins over the cnf, so variables added to the cnf after first boot
		// never take effect on persistence-enabled clusters. With --reload,
		// ProxySQL loads proxysql.db into memory, then applies each cnf entry
		// via INSERT OR REPLACE (cnf wins for keys present in both; db-only
		// entries survive), and saves the merged result back to disk. Keys
		// REMOVED from the cnf still keep their db value — that caveat stands.
		// Harmless when persistence is off (fresh datadir every start).
		Args: []string{
			"-f",
			"-c", "/etc/proxysql/proxysql.cnf",
			"-D", "/var/lib/proxysql",
			"--reload",
		},
		Ports:          ports,
		StartupProbe:   b.startupProbe(),
		LivenessProbe:  b.livenessProbe(),
		ReadinessProbe: b.readinessProbe(),
		Resources:      b.Spec.Resources,
		VolumeMounts:   b.proxysqlVolumeMounts(),
	}
}

// startupProbe, livenessProbe, and readinessProbe resolve spec.probes
// against the operator's built-in defaults. A set override REPLACES the
// default probe wholesale (see ProbesSpec doc); an unset field keeps the
// hardcoded default exactly as it was before spec.probes existed — this is
// what keeps TestGolden (issue #58) stable for specs that don't set probes.
func (b *Builder) startupProbe() *corev1.Probe {
	// No default startup probe: ProxySQL boots fast and has no dependency
	// wait, so this stays nil unless the user opts in.
	return b.Spec.Probes.Startup
}

func (b *Builder) livenessProbe() *corev1.Probe {
	if b.Spec.Probes.Liveness != nil {
		return b.Spec.Probes.Liveness
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(portNameAdmin)},
		},
		InitialDelaySeconds: 15,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

func (b *Builder) readinessProbe() *corev1.Probe {
	if b.Spec.Probes.Readiness != nil {
		return b.Spec.Probes.Readiness
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(portNameAdmin)},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		FailureThreshold:    3,
	}
}

// proxysqlVolumeMounts returns the main container's mounts. The logs
// emptyDir is added only when the logging sidecar is enabled: ProxySQL
// writes the eventslog there and Fluent Bit tails it.
func (b *Builder) proxysqlVolumeMounts() []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/etc/proxysql", ReadOnly: true},
		{Name: "data", MountPath: "/var/lib/proxysql"},
	}
	if b.LoggingEnabled() {
		mounts = append(mounts, corev1.VolumeMount{Name: "logs", MountPath: logsMountPath})
	}
	if b.Spec.TLSEnabled() {
		// The tls mount is load-bearing at runtime, not just for the init
		// step: the datadir symlinks resolve inside THIS container's
		// filesystem, so proxysql (and RELOAD TLS re-reads) need the
		// Secret projected here.
		mounts = append(mounts, corev1.VolumeMount{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true})
		if b.backendTLSEnabled() {
			mounts = append(mounts, corev1.VolumeMount{Name: backendTLSVolumeName, MountPath: backendTLSMountPath, ReadOnly: true})
		}
	}
	return mounts
}

func (b *Builder) dataPVC() corev1.PersistentVolumeClaim {
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: b.Spec.Persistence.AccessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: b.Spec.Persistence.Size,
				},
			},
		},
	}
	if b.Spec.Persistence.StorageClass != nil {
		pvc.Spec.StorageClassName = b.Spec.Persistence.StorageClass
	}
	return pvc
}

// keepaliveSysctls renders spec.networking.tcpKeepalive into pod-level
// sysctls. All three are in the Kubernetes safe-sysctl set since v1.29
// (KEP-3105), so they are admitted under PSA `restricted` without any
// kubelet --allowed-unsafe-sysctls configuration.
func (b *Builder) keepaliveSysctls() []corev1.Sysctl {
	ka := b.Spec.Networking.TCPKeepalive
	var out []corev1.Sysctl
	if ka.Time != nil {
		out = append(out, corev1.Sysctl{Name: "net.ipv4.tcp_keepalive_time", Value: strconv.Itoa(int(*ka.Time))})
	}
	if ka.Interval != nil {
		out = append(out, corev1.Sysctl{Name: "net.ipv4.tcp_keepalive_intvl", Value: strconv.Itoa(int(*ka.Interval))})
	}
	if ka.Probes != nil {
		out = append(out, corev1.Sysctl{Name: "net.ipv4.tcp_keepalive_probes", Value: strconv.Itoa(int(*ka.Probes))})
	}
	return out
}

func ptrInt64(v int64) *int64 { return &v }

// isTrue reports whether a *bool is non-nil and dereferences to true.
func isTrue(p *bool) bool { return p != nil && *p }

// boolPtr returns a pointer to v.
func boolPtr(v bool) *bool { return &v }
