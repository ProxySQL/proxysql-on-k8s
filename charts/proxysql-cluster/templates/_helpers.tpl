{{/* vim: set filetype=mustache: */}}

{{- define "proxysql-cluster.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "proxysql-cluster.fullname" -}}
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

{{- define "proxysql-cluster.headlessName" -}}
{{- printf "%s-headless" (include "proxysql-cluster.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "proxysql-cluster.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "proxysql-cluster.labels" -}}
helm.sh/chart: {{ include "proxysql-cluster.chart" . }}
{{ include "proxysql-cluster.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: proxysql-cluster
{{- end -}}

{{- define "proxysql-cluster.selectorLabels" -}}
app.kubernetes.io/name: {{ include "proxysql-cluster.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "proxysql-cluster.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{- define "proxysql-cluster.passwords" -}}
{{- $existing := (lookup "v1" "Secret" .Release.Namespace (include "proxysql-cluster.fullname" .)) | default (dict "data" (dict)) -}}
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
Escape a string for safe interpolation into a double-quoted ProxySQL
(libconfig) .cnf value: backslash -> \\, then double-quote -> \". Order
matters — backslashes must be escaped first.
*/}}
{{- define "proxysql-cluster.cnfEscape" -}}
{{- . | replace "\\" "\\\\" | replace "\"" "\\\"" -}}
{{- end -}}

{{- define "proxysql-cluster.variableLine" -}}
{{- $k := .key -}}
{{- $v := .value -}}
{{- if kindIs "string" $v -}}
{{ $k }}="{{ include "proxysql-cluster.cnfEscape" $v }}"
{{- else if kindIs "bool" $v -}}
{{ $k }}={{ $v }}
{{- else if kindIs "float64" $v -}}
{{ $k }}={{ int64 $v }}
{{- else -}}
{{ $k }}={{ $v }}
{{- end -}}
{{- end -}}
