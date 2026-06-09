{{/* vim: set filetype=mustache: */}}

{{- define "proxysql.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "proxysql.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "proxysql.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "proxysql.labels" -}}
helm.sh/chart: {{ include "proxysql.chart" . }}
{{ include "proxysql.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: proxysql
{{- end -}}

{{- define "proxysql.selectorLabels" -}}
app.kubernetes.io/name: {{ include "proxysql.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "proxysql.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Resolve admin/radmin/monitor passwords with this precedence:
  1. value explicitly set in values (.Values.auth.*Password)
  2. existing chart-managed Secret (preserves passwords across upgrade)
  3. randomly generate a 32-char password
Returns a dict with keys: admin, radmin, monitor (raw plaintext).
*/}}
{{- define "proxysql.passwords" -}}
{{- $existing := (lookup "v1" "Secret" .Release.Namespace (include "proxysql.fullname" .)) | default (dict "data" (dict)) -}}
{{- $data := $existing.data | default (dict) -}}
{{- $admin := .Values.auth.adminPassword -}}
{{- if not $admin -}}{{- $admin = (get $data "admin-password" | default "" | b64dec) -}}{{- end -}}
{{- if not $admin -}}{{- $admin = randAlphaNum 32 -}}{{- end -}}
{{- $radmin := .Values.auth.radminPassword -}}
{{- if not $radmin -}}{{- $radmin = (get $data "radmin-password" | default "" | b64dec) -}}{{- end -}}
{{- if not $radmin -}}{{- $radmin = randAlphaNum 32 -}}{{- end -}}
{{- $monitor := .Values.auth.monitorPassword -}}
{{- if not $monitor -}}{{- $monitor = (get $data "monitor-password" | default "" | b64dec) -}}{{- end -}}
{{- if not $monitor -}}{{- $monitor = randAlphaNum 32 -}}{{- end -}}
{{- dict "admin" $admin "radmin" $radmin "monitor" $monitor | toYaml -}}
{{- end -}}

{{/*
Render a single ProxySQL variable line. Strings get quoted, numbers/bools don't.
*/}}
{{- define "proxysql.variableLine" -}}
{{- $k := .key -}}
{{- $v := .value -}}
{{- if kindIs "string" $v -}}
{{ $k }}="{{ $v }}"
{{- else if kindIs "bool" $v -}}
{{ $k }}={{ $v }}
{{- else if kindIs "float64" $v -}}
{{ $k }}={{ int64 $v }}
{{- else -}}
{{ $k }}={{ $v }}
{{- end -}}
{{- end -}}
