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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// Key/sink-name constants local to these tests (goconst).
const (
	sinkS3       = "s3"
	sinkHTTP     = "http"
	proxysqlCnf  = "proxysql.cnf"
	fluentBitCnf = "fluent-bit.conf"
)

// loggingOn returns a mutator enabling the sidecar with the query-log input
// (the only valid combination per the CRD's CEL rule).
func loggingOn(mut ...func(*proxysqlv1alpha1.LoggingSpec)) func(*proxysqlv1alpha1.ProxySQLCluster) {
	return func(c *proxysqlv1alpha1.ProxySQLCluster) {
		c.Spec.Logging = &proxysqlv1alpha1.LoggingSpec{Enabled: true, QueryLog: true}
		for _, m := range mut {
			m(c.Spec.Logging)
		}
	}
}

func TestDefaultedSpec_Logging_NilStaysNil(t *testing.T) {
	spec := DefaultedSpec(newCluster("c"))
	if spec.Logging != nil {
		t.Errorf("logging should default to nil (sidecar off), got %+v", spec.Logging)
	}
}

func TestDefaultedSpec_Logging_Defaults(t *testing.T) {
	spec := DefaultedSpec(newCluster("c", loggingOn()))
	l := spec.Logging
	if l == nil {
		t.Fatal("logging spec lost during defaulting")
	}
	if l.SinkType != "stdout" {
		t.Errorf("sinkType default = %q, want stdout", l.SinkType)
	}
	if l.Image != DefaultFluentBitImage {
		t.Errorf("image default = %q, want %q", l.Image, DefaultFluentBitImage)
	}
	if l.BufferSize.String() != DefaultLogBufferSize {
		t.Errorf("bufferSize default = %s, want %s", l.BufferSize.String(), DefaultLogBufferSize)
	}
	if l.Resources.Requests.Cpu().String() != "50m" || l.Resources.Requests.Memory().String() != "64Mi" {
		t.Errorf("resource requests default = %v, want 50m/64Mi", l.Resources.Requests)
	}
	if l.Resources.Limits.Cpu().String() != "200m" || l.Resources.Limits.Memory().String() != "128Mi" {
		t.Errorf("resource limits default = %v, want 200m/128Mi", l.Resources.Limits)
	}
}

func TestDefaultedSpec_Logging_SinkDefaults(t *testing.T) {
	// s3: prefix defaults to /proxysql/<cluster>.
	c := newCluster(clusterName, loggingOn(func(l *proxysqlv1alpha1.LoggingSpec) {
		l.SinkType = sinkS3
		l.S3 = &proxysqlv1alpha1.S3SinkSpec{
			Bucket:               "audit",
			Region:               "eu-west-1",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "s3-creds"},
		}
	}))
	spec := DefaultedSpec(c)
	if got := spec.Logging.S3.Prefix; got != "/proxysql/"+clusterName {
		t.Errorf("s3 prefix default = %q, want /proxysql/%s", got, clusterName)
	}

	// http: port defaults to 80 (443 with tls), uri to "/".
	c = newCluster(clusterName, loggingOn(func(l *proxysqlv1alpha1.LoggingSpec) {
		l.SinkType = sinkHTTP
		l.HTTP = &proxysqlv1alpha1.HTTPSinkSpec{Host: "collector.example"}
	}))
	spec = DefaultedSpec(c)
	if spec.Logging.HTTP.Port != 80 || spec.Logging.HTTP.URI != "/" {
		t.Errorf("http defaults = port %d uri %q, want 80 /", spec.Logging.HTTP.Port, spec.Logging.HTTP.URI)
	}
	c = newCluster(clusterName, loggingOn(func(l *proxysqlv1alpha1.LoggingSpec) {
		l.SinkType = sinkHTTP
		l.HTTP = &proxysqlv1alpha1.HTTPSinkSpec{Host: "collector.example", TLS: true}
	}))
	spec = DefaultedSpec(c)
	if spec.Logging.HTTP.Port != 443 {
		t.Errorf("http+tls port default = %d, want 443", spec.Logging.HTTP.Port)
	}
}

func TestBootstrapCnf_QueryLogVariables(t *testing.T) {
	b := New(newCluster(clusterName, loggingOn()), newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	cnf, err := b.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	// The eventslog variables live in mysql_variables (MySQL-protocol queries).
	mysqlIdx := strings.Index(cnf, "mysql_variables=")
	if mysqlIdx < 0 {
		t.Fatalf("cnf missing mysql_variables block:\n%s", cnf)
	}
	block := cnf[mysqlIdx:]
	for _, want := range []string{
		`eventslog_filename="/var/log/proxysql/queries"`,
		"eventslog_default_log=1",
		"eventslog_format=2",
		"eventslog_filesize=52428800",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("cnf missing %q:\n%s", want, cnf)
		}
	}

	// And absent when logging is off.
	b2 := New(newCluster(clusterName), newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	cnf2, err := b2.BootstrapCnf(nil)
	if err != nil {
		t.Fatalf("BootstrapCnf: %v", err)
	}
	if strings.Contains(cnf2, "eventslog") {
		t.Errorf("cnf must not mention eventslog when logging is disabled:\n%s", cnf2)
	}
}

func TestFluentBitConf_Stdout(t *testing.T) {
	b := New(newCluster(clusterName, loggingOn()), newScheme(t), Passwords{})
	conf, err := b.FluentBitConf()
	if err != nil {
		t.Fatalf("FluentBitConf: %v", err)
	}
	for _, want := range []string{
		"[SERVICE]",
		"storage.path", "/var/log/proxysql/flb-storage",
		"parsers_file", "/fluent-bit/etc/parsers.conf",
		"[INPUT]",
		"name", "tail",
		"path", "/var/log/proxysql/queries*",
		"parser", "json",
		"db", "/var/log/proxysql/flb-tail.db",
		"storage.type", "filesystem",
		"[OUTPUT]",
		"stdout",
		"match", "*",
		"format", "json_lines",
		"storage.total_limit_size", "1024M", // 1Gi default buffer
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("fluent-bit.conf missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "${") {
		t.Errorf("stdout sink must not reference env vars:\n%s", conf)
	}
}

func TestFluentBitConf_S3(t *testing.T) {
	c := newCluster(clusterName, loggingOn(func(l *proxysqlv1alpha1.LoggingSpec) {
		l.SinkType = sinkS3
		l.BufferSize = resource.MustParse("512Mi")
		l.S3 = &proxysqlv1alpha1.S3SinkSpec{
			Bucket:               "audit",
			Region:               "eu-west-1",
			Endpoint:             "https://minio.minio.svc:9000",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "s3-creds"},
		}
	}))
	b := New(c, newScheme(t), Passwords{})
	conf, err := b.FluentBitConf()
	if err != nil {
		t.Fatalf("FluentBitConf: %v", err)
	}
	for _, want := range []string{
		"name", sinkS3,
		"bucket", "audit",
		"region", "eu-west-1",
		"endpoint", "https://minio.minio.svc:9000",
		"s3_key_format", "/proxysql/" + clusterName + "/",
		// store_dir must live on the writable logs emptyDir (RO rootfs).
		"store_dir", "/var/log/proxysql/flb-s3",
		"storage.total_limit_size", "512M",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("s3 fluent-bit.conf missing %q:\n%s", want, conf)
		}
	}
	// Credentials never land in the file — they arrive via env.
	if strings.Contains(conf, "s3-creds") || strings.Contains(conf, "access_key") {
		t.Errorf("s3 credentials must not be referenced in the config file:\n%s", conf)
	}
}

func TestFluentBitConf_HTTP(t *testing.T) {
	c := newCluster(clusterName, loggingOn(func(l *proxysqlv1alpha1.LoggingSpec) {
		l.SinkType = sinkHTTP
		l.HTTP = &proxysqlv1alpha1.HTTPSinkSpec{
			Host:               "collector.example",
			TLS:                true,
			URI:                "/ingest",
			AuthTokenSecretRef: &corev1.LocalObjectReference{Name: "collector-token"},
		}
	}))
	b := New(c, newScheme(t), Passwords{})
	conf, err := b.FluentBitConf()
	if err != nil {
		t.Fatalf("FluentBitConf: %v", err)
	}
	for _, want := range []string{
		"name", sinkHTTP,
		"host", "collector.example",
		"port", "443", // tls=true defaults the port to 443
		"uri", "/ingest",
		"tls", "on",
		"header", "Authorization Bearer ${FLB_HTTP_TOKEN}",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("http fluent-bit.conf missing %q:\n%s", want, conf)
		}
	}
	// Token never inline.
	if strings.Contains(conf, "collector-token") {
		t.Errorf("http auth token secret must not be referenced in the config file:\n%s", conf)
	}

	// Without a token ref there is no Authorization header.
	c2 := newCluster(clusterName, loggingOn(func(l *proxysqlv1alpha1.LoggingSpec) {
		l.SinkType = sinkHTTP
		l.HTTP = &proxysqlv1alpha1.HTTPSinkSpec{Host: "collector.example"}
	}))
	conf2, err := New(c2, newScheme(t), Passwords{}).FluentBitConf()
	if err != nil {
		t.Fatalf("FluentBitConf: %v", err)
	}
	if strings.Contains(conf2, "Authorization") {
		t.Errorf("no token ref -> no Authorization header:\n%s", conf2)
	}
	if !strings.Contains(conf2, "tls") || !strings.Contains(conf2, "off") {
		t.Errorf("tls=false should render tls off:\n%s", conf2)
	}
}

func TestCnfSecret_CarriesFluentBitConf(t *testing.T) {
	b := New(newCluster(clusterName, loggingOn()), newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	sec, err := b.CnfSecret()
	if err != nil {
		t.Fatalf("CnfSecret: %v", err)
	}
	if len(sec.Data[fluentBitCnf]) == 0 {
		t.Error("cnf Secret must carry fluent-bit.conf when logging is enabled")
	}
	if len(sec.Data[proxysqlCnf]) == 0 {
		t.Error("proxysql.cnf key must survive alongside fluent-bit.conf")
	}

	// And absent when logging is off.
	b2 := New(newCluster(clusterName), newScheme(t), Passwords{Admin: "a", Radmin: "r", Monitor: "m"})
	sec2, err := b2.CnfSecret()
	if err != nil {
		t.Fatalf("CnfSecret: %v", err)
	}
	if _, ok := sec2.Data[fluentBitCnf]; ok {
		t.Error("cnf Secret must not carry fluent-bit.conf when logging is disabled")
	}
}

func TestCnfChecksum_CoversAllKeysDeterministically(t *testing.T) {
	a := map[string][]byte{proxysqlCnf: []byte("x"), fluentBitCnf: []byte("y")}
	b := map[string][]byte{fluentBitCnf: []byte("y"), proxysqlCnf: []byte("x")}
	if CnfChecksum(a) != CnfChecksum(b) {
		t.Error("checksum must be independent of map iteration order")
	}
	changed := map[string][]byte{proxysqlCnf: []byte("x"), fluentBitCnf: []byte("z")}
	if CnfChecksum(a) == CnfChecksum(changed) {
		t.Error("a fluent-bit.conf change must change the checksum")
	}
	oneKey := map[string][]byte{proxysqlCnf: []byte("x")}
	if CnfChecksum(a) == CnfChecksum(oneKey) {
		t.Error("adding a key must change the checksum")
	}
}

func TestStatefulSet_LoggingDisabled_NoSidecarNoVolumes(t *testing.T) {
	b := New(newCluster(clusterName), newScheme(t), Passwords{})
	pod := b.StatefulSet("sum").Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("logging off: want exactly 1 container, got %d", len(pod.Containers))
	}
	for _, v := range pod.Volumes {
		if v.Name == "logs" || v.Name == "flb-config" {
			t.Errorf("logging off: unexpected volume %q", v.Name)
		}
	}
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.MountPath == logsMountPath {
			t.Errorf("logging off: proxysql must not mount /var/log/proxysql")
		}
	}
}

// loggingPodSpec renders the pod spec for a default cluster with the logging
// sidecar enabled (stdout sink, all defaults).
func loggingPodSpec(t *testing.T) corev1.PodSpec {
	t.Helper()
	b := New(newCluster(clusterName, loggingOn()), newScheme(t), Passwords{})
	return b.StatefulSet("sum").Spec.Template.Spec
}

// findContainer returns the named container or fails the test.
func findContainer(t *testing.T, pod corev1.PodSpec, name string) *corev1.Container {
	t.Helper()
	for i := range pod.Containers {
		if pod.Containers[i].Name == name {
			return &pod.Containers[i]
		}
	}
	t.Fatalf("no %q container in %v", name, pod.Containers)
	return nil
}

func TestStatefulSet_LoggingEnabled_SidecarSecurity(t *testing.T) {
	pod := loggingPodSpec(t)

	if len(pod.Containers) != 2 {
		t.Fatalf("want 2 containers (proxysql + fluent-bit), got %d", len(pod.Containers))
	}
	flb := findContainer(t, pod, "fluent-bit")
	if flb.Image != DefaultFluentBitImage {
		t.Errorf("sidecar image = %q, want %q", flb.Image, DefaultFluentBitImage)
	}

	// PSA restricted, exact fields.
	sc := flb.SecurityContext
	if sc == nil {
		t.Fatal("sidecar must carry a securityContext")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("sidecar runAsNonRoot must be true")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 999 || sc.RunAsGroup == nil || *sc.RunAsGroup != 999 {
		t.Errorf("sidecar must run as uid/gid 999, got %v/%v", sc.RunAsUser, sc.RunAsGroup)
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("sidecar readOnlyRootFilesystem must be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("sidecar allowPrivilegeEscalation must be false")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("sidecar must drop ALL capabilities, got %+v", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("sidecar seccomp must be RuntimeDefault, got %+v", sc.SeccompProfile)
	}

	// No probes — kubelet restart-on-exit suffices, and the sidecar must not
	// gate pod readiness.
	if flb.LivenessProbe != nil || flb.ReadinessProbe != nil {
		t.Error("sidecar must not define probes")
	}

	// Resource defaults.
	if flb.Resources.Requests.Cpu().String() != "50m" || flb.Resources.Limits.Memory().String() != "128Mi" {
		t.Errorf("sidecar resources = %+v, want 50m/64Mi req, 200m/128Mi lim", flb.Resources)
	}
}

func TestStatefulSet_LoggingEnabled_Mounts(t *testing.T) {
	pod := loggingPodSpec(t)
	flb := findContainer(t, pod, "fluent-bit")

	// Mounts: logs (rw) + the fluent-bit.conf item from the cnf Secret.
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range flb.VolumeMounts {
		mounts[m.Name] = m
	}
	if m, ok := mounts["logs"]; !ok || m.MountPath != logsMountPath || m.ReadOnly {
		t.Errorf("sidecar logs mount wrong: %+v", mounts["logs"])
	}
	if m, ok := mounts["flb-config"]; !ok ||
		m.MountPath != "/fluent-bit/etc/fluent-bit.conf" || m.SubPath != fluentBitCnf || !m.ReadOnly {
		t.Errorf("sidecar config mount wrong: %+v", mounts["flb-config"])
	}

	// The proxysql container shares the logs emptyDir at the same path.
	psql := findContainer(t, pod, "proxysql")
	found := false
	for _, m := range psql.VolumeMounts {
		if m.Name == "logs" && m.MountPath == logsMountPath && !m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Errorf("proxysql container must mount logs at /var/log/proxysql: %+v", psql.VolumeMounts)
	}
}

func TestStatefulSet_LoggingEnabled_Volumes(t *testing.T) {
	pod := loggingPodSpec(t)

	// Volumes: logs emptyDir bounded by bufferSize; flb-config projects only
	// the fluent-bit.conf key out of the cnf Secret.
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		vols[v.Name] = v
	}
	logs, ok := vols["logs"]
	if !ok || logs.EmptyDir == nil {
		t.Fatalf("logs volume missing or not emptyDir: %+v", vols["logs"])
	}
	if logs.EmptyDir.SizeLimit == nil || logs.EmptyDir.SizeLimit.String() != DefaultLogBufferSize {
		t.Errorf("logs emptyDir sizeLimit = %v, want %s", logs.EmptyDir.SizeLimit, DefaultLogBufferSize)
	}
	fc, ok := vols["flb-config"]
	if !ok || fc.Secret == nil || fc.Secret.SecretName != clusterName+"-cnf" {
		t.Fatalf("flb-config volume must source the cnf Secret: %+v", vols["flb-config"])
	}
	if len(fc.Secret.Items) != 1 || fc.Secret.Items[0].Key != fluentBitCnf {
		t.Errorf("flb-config must project only fluent-bit.conf, got %+v", fc.Secret.Items)
	}
	// The proxysql config volume keeps projecting only proxysql.cnf.
	if cfg := vols["config"]; len(cfg.Secret.Items) != 1 || cfg.Secret.Items[0].Key != proxysqlCnf {
		t.Errorf("config volume must keep projecting only proxysql.cnf, got %+v", cfg.Secret.Items)
	}
}

func TestStatefulSet_LoggingSinkEnv(t *testing.T) {
	// s3: AWS credential env vars from the referenced Secret.
	c := newCluster(clusterName, loggingOn(func(l *proxysqlv1alpha1.LoggingSpec) {
		l.SinkType = sinkS3
		l.S3 = &proxysqlv1alpha1.S3SinkSpec{
			Bucket: "audit", Region: "eu-west-1",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "s3-creds"},
		}
	}))
	pod := New(c, newScheme(t), Passwords{}).StatefulSet("sum").Spec.Template.Spec
	env := sidecarEnv(t, pod)
	for name, key := range map[string]string{
		"AWS_ACCESS_KEY_ID":     "access-key-id",
		"AWS_SECRET_ACCESS_KEY": "secret-access-key",
	} {
		e, ok := env[name]
		if !ok || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
			e.ValueFrom.SecretKeyRef.Name != "s3-creds" || e.ValueFrom.SecretKeyRef.Key != key {
			t.Errorf("env %s must come from secret s3-creds key %s, got %+v", name, key, env[name])
		}
	}

	// http with token: FLB_HTTP_TOKEN from the referenced Secret (key: token).
	c = newCluster(clusterName, loggingOn(func(l *proxysqlv1alpha1.LoggingSpec) {
		l.SinkType = sinkHTTP
		l.HTTP = &proxysqlv1alpha1.HTTPSinkSpec{
			Host:               "collector.example",
			AuthTokenSecretRef: &corev1.LocalObjectReference{Name: "collector-token"},
		}
	}))
	pod = New(c, newScheme(t), Passwords{}).StatefulSet("sum").Spec.Template.Spec
	env = sidecarEnv(t, pod)
	e, ok := env["FLB_HTTP_TOKEN"]
	if !ok || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
		e.ValueFrom.SecretKeyRef.Name != "collector-token" || e.ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("FLB_HTTP_TOKEN must come from secret collector-token key token, got %+v", env["FLB_HTTP_TOKEN"])
	}

	// stdout: no env at all.
	pod = New(newCluster(clusterName, loggingOn()), newScheme(t), Passwords{}).StatefulSet("sum").Spec.Template.Spec
	if env = sidecarEnv(t, pod); len(env) != 0 {
		t.Errorf("stdout sink must not inject env, got %v", env)
	}
}

func sidecarEnv(t *testing.T, pod corev1.PodSpec) map[string]corev1.EnvVar {
	t.Helper()
	for _, c := range pod.Containers {
		if c.Name == "fluent-bit" {
			out := map[string]corev1.EnvVar{}
			for _, e := range c.Env {
				out[e.Name] = e
			}
			return out
		}
	}
	t.Fatal("no fluent-bit container")
	return nil
}
