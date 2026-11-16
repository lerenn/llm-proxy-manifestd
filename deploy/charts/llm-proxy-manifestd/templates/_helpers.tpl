{{- define "llm-proxy-manifestd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "llm-proxy-manifestd.fullname" -}}
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

{{- define "llm-proxy-manifestd.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "llm-proxy-manifestd.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "llm-proxy-manifestd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llm-proxy-manifestd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Providers: one CLI adapter sidecar per entry of .Values.providers.
Returns a JSON list [{name, type, port, maxConcurrency, ...}] sorted by name.
Ports are deterministic: providersBasePort + index in name order, unless the
provider sets `port` explicitly.
*/}}
{{- define "llm-proxy-manifestd.providers" -}}
{{- if .Values.accounts -}}
{{- fail "the `accounts` value was renamed to `providers` (also accountsBasePort -> providersBasePort, accountStorage -> providerStorage): please rename it in your values" -}}
{{- end -}}
{{- if or .Values.accountsBasePort .Values.accountStorage -}}
{{- fail "`accountsBasePort`/`accountStorage` were renamed to `providersBasePort`/`providerStorage`: please rename them in your values" -}}
{{- end -}}
{{- $out := list -}}
{{- $base := int (.Values.providersBasePort | default 9100) -}}
{{- $names := keys (.Values.providers | default dict) | sortAlpha -}}
{{- range $i, $name := $names -}}
{{- $a := get $.Values.providers $name | default dict -}}
{{- if not (regexMatch "^[a-z]([-a-z0-9]{0,28}[a-z0-9])?$" $name) -}}
{{- fail (printf "providers.%s: name must be a DNS label of at most 30 characters (lowercase letters, digits, '-')" $name) -}}
{{- end -}}
{{- if ne ($a.type | default "") "claude" -}}
{{- fail (printf "providers.%s: unsupported type %q (supported: claude)" $name ($a.type | default "")) -}}
{{- end -}}
{{- $port := int ($a.port | default (add $base $i)) -}}
{{- $out = append $out (dict "name" $name "type" $a.type "port" $port "maxConcurrency" (int ($a.maxConcurrency | default 4)) "provider" $a) -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}
