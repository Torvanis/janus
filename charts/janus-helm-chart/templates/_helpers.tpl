{{- define "janus.name" -}}
{{- default "janus" .Values.nameOverride | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- define "janus.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "janus.name" .) | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- define "janus.selector" -}}
app.kubernetes.io/name: {{ include "janus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: gateway
{{- end -}}
{{- define "janus.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{ include "janus.selector" . }}
{{- end -}}
{{- define "janus.pgSelector" -}}
app.kubernetes.io/name: {{ include "janus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: postgresql
{{- end -}}
{{/*
"true" when an auto/true/false setting is on, "" otherwise.
Usage: include "janus.autoEnabled" (list .Values.x.enabled .Values.replicaCount)
*/}}
{{- define "janus.autoEnabled" -}}
{{- $setting := index . 0 -}}
{{- if kindIs "bool" $setting -}}
{{- if $setting }}true{{ end -}}
{{- else if gt (int (index . 1)) 1 -}}
true
{{- end -}}
{{- end -}}
{{- define "janus.preStopSleep" -}}
{{- if and (gt (int .Values.lifecycle.preStopSleepSeconds) 0) (semverCompare ">=1.30.0-0" .Capabilities.KubeVersion.Version) -}}
true
{{- end -}}
{{- end -}}
{{- define "janus.image" -}}
{{- if .Values.image.digest -}}
{{ printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else -}}
{{ printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- end -}}
{{- end -}}
