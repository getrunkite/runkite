{{/*
Expand the name of the chart.
*/}}
{{- define "runkite.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "runkite.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart label.
*/}}
{{- define "runkite.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "runkite.labels" -}}
helm.sh/chart: {{ include "runkite.chart" . }}
{{ include "runkite.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels for the control plane.
*/}}
{{- define "runkite.selectorLabels" -}}
app.kubernetes.io/name: {{ include "runkite.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: control-plane
{{- end }}

{{/*
Selector labels for the runner.
*/}}
{{- define "runkite.runner.selectorLabels" -}}
app.kubernetes.io/name: {{ include "runkite.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: runner
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "runkite.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "runkite.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Secret name holding DSNs / runner token.
*/}}
{{- define "runkite.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else }}
{{- include "runkite.fullname" . }}
{{- end }}
{{- end }}

{{/*
Validate tls.* when enabled. Fails helm template/install early.
*/}}
{{- define "runkite.tls.validate" -}}
{{- if .Values.tls.enabled }}
{{- if not .Values.tls.serverSecretName }}
{{- fail "tls.enabled requires tls.serverSecretName (kubernetes.io/tls with tls.crt + tls.key)" }}
{{- end }}
{{- if not .Values.tls.caSecretName }}
{{- fail "tls.enabled requires tls.caSecretName (Secret key ca.crt)" }}
{{- end }}
{{- if or .Values.tls.http.mtls .Values.tls.grpc.mtls }}
{{- if not .Values.tls.clientSecretName }}
{{- fail "tls.*.mtls requires tls.clientSecretName (kubernetes.io/tls with runner client cert)" }}
{{- end }}
{{- end }}
{{- if and .Values.tls.http.mtls (not .Values.tls.http.enabled) }}
{{- fail "tls.http.mtls requires tls.http.enabled" }}
{{- end }}
{{- if .Values.tls.http.mtls }}
{{- fail "tls.http.mtls is not supported by this chart's default httpGet probes (kubelet cannot present a client cert) -- this will permanently fail health checks" }}
{{- end }}
{{- if and .Values.tls.grpc.mtls (not .Values.tls.grpc.enabled) }}
{{- fail "tls.grpc.mtls requires tls.grpc.enabled" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "runkite.tls.mountPath" -}}
{{- default "/etc/runkite/tls" .Values.tls.mountPath }}
{{- end }}

{{- define "runkite.tls.serverDir" -}}
{{- printf "%s/server" (include "runkite.tls.mountPath" .) }}
{{- end }}

{{- define "runkite.tls.caDir" -}}
{{- printf "%s/ca" (include "runkite.tls.mountPath" .) }}
{{- end }}

{{- define "runkite.tls.clientDir" -}}
{{- printf "%s/client" (include "runkite.tls.mountPath" .) }}
{{- end }}

{{- define "runkite.httpScheme" -}}
{{- if and .Values.tls.enabled .Values.tls.http.enabled -}}https{{- else -}}http{{- end -}}
{{- end }}
