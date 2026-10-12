package builders

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

func TestRDSClusterRendersPluginRuntime(t *testing.T) {
	cluster := newCluster("rds-proxy", func(cluster *proxysqlv1alpha1.ProxySQLCluster) {
		cluster.Spec.ServiceAccountName = "proxysql-rds-workload"
		cluster.Spec.RDS = &proxysqlv1alpha1.RDSRuntimeSpec{
			Region: "us-east-1", AssumeRoleARN: "arn:aws:iam::123456789012:role/rds", ExternalID: "organization-1",
		}
	})
	builder := New(cluster, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})

	if !builder.Spec.Protocols.Web.IsEnabled() || builder.Spec.Protocols.Web.Port != DefaultWebPort {
		t.Fatalf("RDS web listener = %+v, want enabled on %d", builder.Spec.Protocols.Web, DefaultWebPort)
	}
	if !builder.Spec.TLSEnabled() {
		t.Fatal("RDS API must have a verifiable serving certificate")
	}
	cnf, err := builder.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	if !strings.Contains(cnf, "\naws_managed=true;\n") {
		t.Fatalf("bootstrap cnf does not enable managed service:\n%s", cnf)
	}

	statefulSet := builder.StatefulSet("checksum")
	pod := statefulSet.Spec.Template.Spec
	if pod.ServiceAccountName != "proxysql-rds-workload" {
		t.Fatalf("serviceAccountName = %q", pod.ServiceAccountName)
	}
	container := pod.Containers[0]
	if !slices.Equal(container.Command, []string{"/usr/local/bin/proxysql-entrypoint"}) {
		t.Fatalf("command = %v", container.Command)
	}
	wantBootstrapArg := "--aws-managed-bootstrap=" + RDSBootstrapMountPath + "/" + RDSBootstrapFileName
	if !slices.Contains(container.Args, wantBootstrapArg) {
		t.Fatalf("args = %v, missing %q", container.Args, wantBootstrapArg)
	}
	if !hasVolume(pod.Volumes, RDSBootstrapVolumeName, builder.RDSBootstrapSecretName()) {
		t.Fatalf("RDS bootstrap volume missing from %+v", pod.Volumes)
	}
	if !hasMount(container.VolumeMounts, RDSBootstrapVolumeName, RDSBootstrapMountPath) {
		t.Fatalf("RDS bootstrap mount missing from %+v", container.VolumeMounts)
	}

	service := builder.Service()
	if !slices.ContainsFunc(service.Spec.Ports, func(port corev1.ServicePort) bool {
		return port.Name == portNameWeb && port.Port == DefaultWebPort
	}) {
		t.Fatalf("RDS web service port missing from %+v", service.Spec.Ports)
	}
}

func hasVolume(volumes []corev1.Volume, name, secretName string) bool {
	return slices.ContainsFunc(volumes, func(volume corev1.Volume) bool {
		return volume.Name == name && volume.Secret != nil && volume.Secret.SecretName == secretName
	})
}

func hasMount(mounts []corev1.VolumeMount, name, path string) bool {
	return slices.ContainsFunc(mounts, func(mount corev1.VolumeMount) bool {
		return mount.Name == name && mount.MountPath == path && mount.ReadOnly
	})
}
