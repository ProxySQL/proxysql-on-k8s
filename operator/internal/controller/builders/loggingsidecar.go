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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
)

// Fluent Bit sidecar defaults (see docs/superpowers/specs/2026-06-10-logging-
// sidecar-design.md). The image is a pinned tag, bumped deliberately via PRs.
const (
	DefaultFluentBitImage = "fluent/fluent-bit:4.0.3"
	DefaultLogBufferSize  = "1Gi"

	// logsMountPath is the dedicated logs emptyDir, shared by both containers:
	// ProxySQL writes the eventslog there, Fluent Bit tails it and keeps its
	// position DB and filesystem buffer on the same volume (its only writable
	// path under readOnlyRootFilesystem).
	logsMountPath = "/var/log/proxysql"
)

// fluentBitConfTemplate renders the sidecar config. It deliberately contains
// NO secrets: S3/HTTP credentials reach the sidecar as env vars from
// secretKeyRef and are referenced as ${...}, expanded by Fluent Bit at
// startup. Every output gets storage.total_limit_size derived from
// bufferSize so a sink outage buffers to the emptyDir up to a bound, then
// drops the oldest chunks.
const fluentBitConfTemplate = `# Operator-managed Fluent Bit config for ProxySQLCluster {{ .ClusterName }}.
[SERVICE]
    flush                     1
    parsers_file              /fluent-bit/etc/parsers.conf
    storage.path              {{ .LogsPath }}/flb-storage

[INPUT]
    name                      tail
    path                      {{ .LogsPath }}/queries*
    parser                    json
    db                        {{ .LogsPath }}/flb-tail.db
    storage.type              filesystem

{{ if eq .SinkType "s3" -}}
[OUTPUT]
    name                      s3
    match                     *
    bucket                    {{ .S3Bucket }}
    region                    {{ .S3Region }}
{{- if .S3Endpoint }}
    endpoint                  {{ .S3Endpoint }}
{{- end }}
    s3_key_format             {{ .S3Prefix }}/%Y/%m/%d/%H%M%S-$UUID.jsonl
    store_dir                 {{ .LogsPath }}/flb-s3
    storage.total_limit_size  {{ .TotalLimitSize }}
{{ else if eq .SinkType "http" -}}
[OUTPUT]
    name                      http
    match                     *
    host                      {{ .HTTPHost }}
    port                      {{ .HTTPPort }}
    uri                       {{ .HTTPURI }}
    format                    json_lines
    tls                       {{ if .HTTPTLS }}on{{ else }}off{{ end }}
{{- if .HTTPAuthToken }}
    header                    Authorization Bearer ${FLB_HTTP_TOKEN}
{{- end }}
    storage.total_limit_size  {{ .TotalLimitSize }}
{{ else -}}
[OUTPUT]
    name                      stdout
    match                     *
    format                    json_lines
    storage.total_limit_size  {{ .TotalLimitSize }}
{{ end -}}
`

// fluentBitConfData is the input to fluentBitConfTemplate.
type fluentBitConfData struct {
	ClusterName    string
	LogsPath       string
	SinkType       string
	TotalLimitSize string
	S3Bucket       string
	S3Region       string
	S3Endpoint     string
	S3Prefix       string
	HTTPHost       string
	HTTPPort       int32
	HTTPURI        string
	HTTPTLS        bool
	HTTPAuthToken  bool
}

// LoggingEnabled reports whether the Fluent Bit sidecar is requested.
func (b *Builder) LoggingEnabled() bool {
	return b.Spec.Logging != nil && b.Spec.Logging.Enabled
}

// FluentBitConf renders the sidecar config for the defaulted logging spec.
// Pure: no secrets are embedded (sink credentials arrive via env vars).
func (b *Builder) FluentBitConf() (string, error) {
	l := b.Spec.Logging
	if l == nil {
		return "", fmt.Errorf("logging spec is nil")
	}
	tpl, err := template.New("fluent-bit.conf").Parse(fluentBitConfTemplate)
	if err != nil {
		return "", fmt.Errorf("parse fluent-bit template: %w", err)
	}
	data := fluentBitConfData{
		ClusterName:    b.Name(),
		LogsPath:       logsMountPath,
		SinkType:       l.SinkType,
		TotalLimitSize: fluentBitSize(l.BufferSize.Value()),
	}
	if l.S3 != nil {
		data.S3Bucket = l.S3.Bucket
		data.S3Region = l.S3.Region
		data.S3Endpoint = l.S3.Endpoint
		data.S3Prefix = strings.TrimSuffix(l.S3.Prefix, "/")
	}
	if l.HTTP != nil {
		data.HTTPHost = l.HTTP.Host
		data.HTTPPort = l.HTTP.Port
		data.HTTPURI = l.HTTP.URI
		data.HTTPTLS = l.HTTP.TLS
		data.HTTPAuthToken = l.HTTP.AuthTokenSecretRef != nil
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render fluent-bit.conf: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n") + "\n", nil
}

// fluentBitSize converts a byte count into Fluent Bit's size notation,
// rounding up to whole mebibytes (minimum 1M).
func fluentBitSize(n int64) string {
	const mib = 1024 * 1024
	return fmt.Sprintf("%dM", max((n+mib-1)/mib, 1))
}

// fluentBitContainer returns the log-shipping sidecar: a regular container
// (not a native sidecar initContainer — ordering guarantees aren't worth the
// machinery for a log shipper), PSA-restricted, no probes (kubelet
// restart-on-exit suffices and it must not gate pod readiness).
func (b *Builder) fluentBitContainer() corev1.Container {
	l := b.Spec.Logging
	return corev1.Container{
		Name:            "fluent-bit",
		Image:           l.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             boolPtr(true),
			RunAsUser:                ptrInt64(999),
			RunAsGroup:               ptrInt64(999),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: l.Resources,
		Env:       b.fluentBitEnv(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "logs", MountPath: logsMountPath},
			{
				Name: "flb-config",
				// subPath overlays just this file, keeping the image's
				// /fluent-bit/etc/parsers.conf (and default CMD) intact.
				// Stale-on-update doesn't matter: a conf change rolls the
				// pods via the cnf-checksum annotation.
				MountPath: "/fluent-bit/etc/fluent-bit.conf",
				SubPath:   "fluent-bit.conf",
				ReadOnly:  true,
			},
		},
	}
}

// fluentBitEnv wires sink credentials from the referenced Secrets into the
// sidecar. Credentials never appear in fluent-bit.conf — the config
// references ${...} env vars instead.
func (b *Builder) fluentBitEnv() []corev1.EnvVar {
	l := b.Spec.Logging
	var env []corev1.EnvVar
	if l.SinkType == "s3" && l.S3 != nil {
		for name, key := range map[string]string{
			"AWS_ACCESS_KEY_ID":     "access-key-id",
			"AWS_SECRET_ACCESS_KEY": "secret-access-key",
		} {
			env = append(env, corev1.EnvVar{
				Name: name,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: l.S3.CredentialsSecretRef,
						Key:                  key,
					},
				},
			})
		}
		// Deterministic order (map iteration is random).
		slices.SortFunc(env, func(a, c corev1.EnvVar) int { return strings.Compare(a.Name, c.Name) })
	}
	if l.SinkType == "http" && l.HTTP != nil && l.HTTP.AuthTokenSecretRef != nil {
		env = append(env, corev1.EnvVar{
			Name: "FLB_HTTP_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: *l.HTTP.AuthTokenSecretRef,
					Key:                  "token",
				},
			},
		})
	}
	return env
}

// loggingVolumes returns the pod volumes the sidecar needs: the bounded logs
// emptyDir (shared with ProxySQL) and a projection of the fluent-bit.conf key
// out of the cnf Secret.
func (b *Builder) loggingVolumes() []corev1.Volume {
	bufferSize := b.Spec.Logging.BufferSize
	return []corev1.Volume{
		{
			Name: "logs",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &bufferSize},
			},
		},
		{
			Name: "flb-config",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: b.CnfSecretName(),
					Items: []corev1.KeyToPath{
						{Key: "fluent-bit.conf", Path: "fluent-bit.conf"},
					},
				},
			},
		},
	}
}

// CnfChecksum returns a deterministic SHA-256 over every key of the cnf
// Secret (keys sorted, key and value length-prefixed), so a change to any
// key — proxysql.cnf or fluent-bit.conf — rolls the pods via the
// proxysql.com/cnf-checksum annotation.
func CnfChecksum(data map[string][]byte) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(data)) {
		_, _ = fmt.Fprintf(h, "%d:%s:%d:", len(k), k, len(data[k]))
		_, _ = h.Write(data[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
