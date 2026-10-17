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
		cluster.Spec.Plugin = &proxysqlv1alpha1.ClusterPluginSpec{AWS: &proxysqlv1alpha1.AWSClusterPluginSpec{
			Bootstrap: proxysqlv1alpha1.AWSBootstrapSpec{ConfigMapRef: corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "rds-proxy-aws-bootstrap"}, Key: "aws-deployment.json",
			}},
		}}
	})
	builder := New(cluster, newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	builder.AWSBootstrapChecksum = "aws-checksum"

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
	if !strings.Contains(cnf, "\nplugins=(\"/usr/lib/proxysql/ProxySQL_Aws_Plugin.so\");\n") {
		t.Fatalf("bootstrap cnf does not load the AWS plugin bundle:\n%s", cnf)
	}
	if !strings.Contains(cnf, "\nweb_interface_plugin=\"/usr/lib/proxysql/Web_Interface_plugin.so\";\n") {
		t.Fatalf("bootstrap cnf does not load the Web plugin:\n%s", cnf)
	}

	statefulSet := builder.StatefulSet("checksum")
	pod := statefulSet.Spec.Template.Spec
	if pod.ServiceAccountName != "proxysql-rds-workload" {
		t.Fatalf("serviceAccountName = %q", pod.ServiceAccountName)
	}
	container := pod.Containers[0]
	if !slices.Equal(container.Command, []string{"proxysql"}) {
		t.Fatalf("command = %v", container.Command)
	}
	if slices.ContainsFunc(container.Args, func(arg string) bool { return strings.Contains(arg, "aws-managed-bootstrap") }) {
		t.Fatalf("args retain removed bootstrap flag: %v", container.Args)
	}
	if !hasConfigMapVolume(pod.Volumes, AWSBootstrapVolumeName, "rds-proxy-aws-bootstrap") {
		t.Fatalf("AWS bootstrap volume missing from %+v", pod.Volumes)
	}
	if !hasMount(container.VolumeMounts, AWSBootstrapVolumeName, AWSBootstrapMountPath) {
		t.Fatalf("AWS bootstrap mount missing from %+v", container.VolumeMounts)
	}
	if !hasVolume(pod.Volumes, AWSCredentialsVolumeName, builder.AWSCredentialsSecretName()) {
		t.Fatalf("AWS credentials volume missing from %+v", pod.Volumes)
	}
	if statefulSet.Spec.Template.Annotations[AWSBootstrapChecksumAnnotation] != "aws-checksum" {
		t.Fatalf("AWS bootstrap checksum annotation missing from %+v", statefulSet.Spec.Template.Annotations)
	}

	service := builder.Service()
	if !slices.ContainsFunc(service.Spec.Ports, func(port corev1.ServicePort) bool {
		return port.Name == portNameWeb && port.Port == DefaultWebPort
	}) {
		t.Fatalf("RDS web service port missing from %+v", service.Spec.Ports)
	}
}

func hasConfigMapVolume(volumes []corev1.Volume, name, configMapName string) bool {
	return slices.ContainsFunc(volumes, func(volume corev1.Volume) bool {
		return volume.Name == name && volume.ConfigMap != nil && volume.ConfigMap.Name == configMapName
	})
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
