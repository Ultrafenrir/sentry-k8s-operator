{{- define "sentry-kubernetes-operator.image" -}}
{{ printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- end -}}

{{- define "sentry-kubernetes-operator.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sentry-kubernetes-operator.clusterRoleName" -}}
{{- printf "%s-%s-manager" .Release.Namespace .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sentry-kubernetes-operator.labels" -}}
app.kubernetes.io/name: sentry-kubernetes-operator
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "sentry-kubernetes-operator.selectorLabels" -}}
app.kubernetes.io/name: sentry-kubernetes-operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
