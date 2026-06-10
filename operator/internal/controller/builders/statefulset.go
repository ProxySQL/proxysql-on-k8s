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
	"crypto/sha256"
	"encoding/hex"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// StatefulSet builds the desired-state StatefulSet. cnfChecksum is the SHA of
// the rendered proxysql.cnf; included as a pod-template annotation so config
// changes trigger a rolling restart.
func (b *Builder) StatefulSet(cnfChecksum string) *appsv1.StatefulSet {
	labels := b.Labels()
	selector := b.SelectorLabels()

	podLabels := make(map[string]string, len(selector)+len(b.Spec.PodLabels))
	maps.Copy(podLabels, selector)
	maps.Copy(podLabels, b.Spec.PodLabels)

	podAnnotations := map[string]string{
		"proxysql.com/cnf-checksum": cnfChecksum,
	}
	maps.Copy(podAnnotations, b.Spec.PodAnnotations)

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Name(),
			Namespace: b.Namespace(),
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         b.HeadlessName(),
			Replicas:            b.Spec.Replicas,
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

func (b *Builder) podSpec() corev1.PodSpec {
	spec := corev1.PodSpec{
		ImagePullSecrets:              b.Spec.ImagePullSecrets,
		SecurityContext:               b.Spec.PodSecurityContext,
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

	return spec
}

func (b *Builder) container() corev1.Container {
	ports := []corev1.ContainerPort{
		{Name: "admin", ContainerPort: b.Spec.Protocols.Admin.Port, Protocol: corev1.ProtocolTCP},
	}
	if b.Spec.Protocols.MySQL.IsEnabled() {
		ports = append(ports, corev1.ContainerPort{Name: "mysql", ContainerPort: b.Spec.Protocols.MySQL.Port, Protocol: corev1.ProtocolTCP})
	}
	if b.Spec.Protocols.PostgreSQL.IsEnabled() {
		ports = append(ports, corev1.ContainerPort{Name: "pgsql", ContainerPort: b.Spec.Protocols.PostgreSQL.Port, Protocol: corev1.ProtocolTCP})
	}
	if isTrue(b.Spec.Metrics.Enabled) {
		ports = append(ports, corev1.ContainerPort{Name: "metrics", ContainerPort: b.Spec.Metrics.Port, Protocol: corev1.ProtocolTCP})
	}
	if b.Spec.Protocols.Web.IsEnabled() {
		ports = append(ports, corev1.ContainerPort{Name: "web", ContainerPort: b.Spec.Protocols.Web.Port, Protocol: corev1.ProtocolTCP})
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
		Args: []string{
			"-f",
			"-c", "/etc/proxysql/proxysql.cnf",
			"-D", "/var/lib/proxysql",
		},
		Ports: ports,
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("admin")},
			},
			InitialDelaySeconds: 15,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("admin")},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			FailureThreshold:    3,
		},
		Resources: b.Spec.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: "/etc/proxysql", ReadOnly: true},
			{Name: "data", MountPath: "/var/lib/proxysql"},
		},
	}
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

// Sha256 returns the hex SHA-256 of s, used for the cnf checksum annotation.
func Sha256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func ptrInt64(v int64) *int64 { return &v }

// isTrue reports whether a *bool is non-nil and dereferences to true.
func isTrue(p *bool) bool { return p != nil && *p }

// boolPtr returns a pointer to v.
func boolPtr(v bool) *bool { return &v }
