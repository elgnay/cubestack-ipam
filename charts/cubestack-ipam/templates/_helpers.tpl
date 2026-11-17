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

{{/* Which MutatingAdmissionPolicy version this cluster serves, or "" if it serves
     none. Used by the policy template and by NOTES.txt, so the thing that is
     installed and the thing that is described cannot disagree.

     Two things make this less obvious than `.Capabilities.APIVersions.Has`.

     1. The group/version is not the question. Every cluster since 1.16 serves
        admissionregistration.k8s.io/v1, but MutatingAdmissionPolicy is beta on
        1.32-1.35 and lives in v1beta1 only -- so asking about the group/version
        picks v1 on a cluster that cannot serve it and the apply is rejected. The
        Kind has to be part of the test. Hence the semver branch first: v1 means
        1.36+, where the gate is GA and locked on, so v1 is certain.

     2. Whether discovery has run at all changes what the capability set contains,
        and it is not signalled directly. `helm install --dry-run=server` (or
        install/upgrade) gets 259 entries including group/version/Kind; `helm
        template` and `helm install --dry-run=client` get a hardcoded 57 entries
        with NO Kind-level entries and a fabricated KubeVersion. So the Kind test
        alone answers "no" under `helm template`, which would silently render
        nothing -- exactly the pothole this chart elsewhere refuses to fall into.
        apps/v1/Deployment is the canary for which world we are in: every cluster
        this chart can run on serves it.

     Where discovery has run it is authoritative and can veto the semver guess
     (a 1.34-1.35 cluster with the gate off). Where it has not, the semver guess
     stands and a wrong one fails loudly at apply with "no matches for kind",
     which names the actual problem and is the better of the two failures. */}}
{{- define "cubestack-ipam.mapAPIVersion" -}}
{{- $kube := .Capabilities.KubeVersion.Version -}}
{{- $ga := semverCompare ">=1.36-0" $kube -}}
{{- $beta := and (semverCompare ">=1.32-0" $kube) (not $ga) -}}
{{- $api := "" -}}
{{- if $ga -}}
{{- $api = "admissionregistration.k8s.io/v1" -}}
{{- else if $beta -}}
{{- $api = "admissionregistration.k8s.io/v1beta1" -}}
{{- end -}}
{{- if .Capabilities.APIVersions.Has "apps/v1/Deployment" -}}
{{- if not (.Capabilities.APIVersions.Has (printf "%s/MutatingAdmissionPolicy" $api)) -}}
{{- $api = "" -}}
{{- end -}}
{{- end -}}
{{- $api -}}
{{- end -}}

