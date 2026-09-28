{{/* Helm's standard name helpers. The names are derived rather than hardcoded so
     that two releases can coexist in one cluster -- which they can, because the
     leader-election lease is namespaced. */}}

{{- define "cubestack-ipam.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cubestack-ipam.fullname" -}}
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

{{- define "cubestack-ipam.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Labels for every object. Kept off the selector, so they can change without
     a rejected Deployment update. */}}
{{- define "cubestack-ipam.labels" -}}
helm.sh/chart: {{ include "cubestack-ipam.chart" . }}
{{ include "cubestack-ipam.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* The Deployment selector. IMMUTABLE: changing these on an existing release
     requires deleting the Deployment first. */}}
{{- define "cubestack-ipam.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cubestack-ipam.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "cubestack-ipam.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "cubestack-ipam.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "cubestack-ipam.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/* The health-probe port is fixed at 8081 by the binary's default flag value,
     not exposed as a value: nothing else needs to agree on it. */}}
