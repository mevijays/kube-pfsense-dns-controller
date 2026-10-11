{{- define "kpdc.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kpdc.fullname" -}}
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

{{- define "kpdc.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "kpdc.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "kpdc.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kpdc.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kpdc.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kpdc.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Name of the Secret that holds the pfSense API key. */}}
{{- define "kpdc.secretName" -}}
{{- if .Values.pfsense.existingSecret }}{{ .Values.pfsense.existingSecret }}{{ else }}{{ include "kpdc.fullname" . }}{{ end }}
{{- end }}

{{- define "kpdc.secretKey" -}}
{{- if .Values.pfsense.existingSecret }}{{ .Values.pfsense.existingSecretKey }}{{ else }}api-key{{ end }}
{{- end }}

{{/* config.domains accepts a list or a comma separated string. */}}
{{- define "kpdc.domains" -}}
{{- if kindIs "string" .Values.config.domains }}{{ .Values.config.domains }}{{ else }}{{ join "," .Values.config.domains }}{{ end }}
{{- end }}
