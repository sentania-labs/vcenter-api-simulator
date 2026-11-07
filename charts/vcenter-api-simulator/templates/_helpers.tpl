{{/*
Expand the name of the chart.
*/}}
{{- define "vcsim.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "vcsim.fullname" -}}
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

{{- define "vcsim.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "vcsim.labels" -}}
helm.sh/chart: {{ include "vcsim.chart" . }}
{{ include "vcsim.selectorLabels" . }}
app.kubernetes.io/version: {{ include "vcsim.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "vcsim.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vcsim.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "vcsim.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "vcsim.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "vcsim.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{- define "vcsim.image" -}}
{{- printf "%s:%s" .Values.image.repository (include "vcsim.imageTag" .) }}
{{- end }}

{{/*
Name of the Secret holding login credentials, when enabled.
*/}}
{{- define "vcsim.credentialsSecretName" -}}
{{- default (printf "%s-credentials" (include "vcsim.fullname" .)) .Values.credentials.existingSecret }}
{{- end }}

{{/*
Name of the kubernetes.io/tls Secret to serve, or empty for a generated one.
*/}}
{{- define "vcsim.tlsSecretName" -}}
{{- if .Values.tls.existingSecret }}
{{- .Values.tls.existingSecret }}
{{- else if .Values.tls.certManager.enabled }}
{{- default (printf "%s-tls" (include "vcsim.fullname" .)) .Values.tls.certManager.secretName }}
{{- end }}
{{- end }}

{{/*
Every name the generated certificate should carry: the public hostname, the
Service's cluster-local names and any extra tlsNames.
*/}}
{{- define "vcsim.tlsNames" -}}
{{- $names := list }}
{{- if .Values.hostname }}{{ $names = append $names .Values.hostname }}{{ end }}
{{- $svc := include "vcsim.fullname" . }}
{{- $names = append $names $svc }}
{{- $names = append $names (printf "%s.%s" $svc .Release.Namespace) }}
{{- $names = append $names (printf "%s.%s.svc" $svc .Release.Namespace) }}
{{- $names = append $names (printf "%s.%s.svc.cluster.local" $svc .Release.Namespace) }}
{{- range .Values.simulator.tlsNames }}{{ $names = append $names . }}{{ end }}
{{- join "," ($names | uniq) }}
{{- end }}
