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
Providers: entries of .Values.providers. Type claude = CLI adapter sidecar (+PVC);
types anthropic-api / openai-api = pool only (no container).
Returns a JSON list [{name, type, port, maxConcurrency, ...}] sorted by name.
Ports are deterministic: providersBasePort + index among sidecar providers in name order, unless the
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
{{- $sidecars := 0 -}}
{{- range $name := $names -}}
{{- $a := get $.Values.providers $name | default dict -}}
{{- $type := $a.type | default "" -}}
{{- if not (regexMatch "^[a-z]([-a-z0-9]{0,28}[a-z0-9])?$" $name) -}}
{{- fail (printf "providers.%s: name must be a DNS label of at most 30 characters (lowercase letters, digits, '-')" $name) -}}
{{- end -}}
{{- $entry := dict "name" $name "type" $type "maxConcurrency" (int ($a.maxConcurrency | default 4)) "provider" $a -}}
{{- if eq $type "claude" -}}
{{- $_ := set $entry "sidecar" true -}}
{{- $_ := set $entry "port" (int ($a.port | default (add $base $sidecars))) -}}
{{- $sidecars = add1 $sidecars -}}
{{- else if or (eq $type "anthropic-api") (eq $type "openai-api") -}}
{{- $_ := set $entry "sidecar" false -}}
{{- $_ := set $entry "protocol" (ternary "anthropic" "openai" (eq $type "anthropic-api")) -}}
{{- $url := $a.url | default (ternary "https://api.anthropic.com" "https://api.openai.com" (eq $type "anthropic-api")) -}}
{{- $_ := set $entry "url" $url -}}
{{- $sec := dig "apiKey" "existingSecret" dict $a -}}
{{- if not (and $sec.name $sec.key) -}}
{{- fail (printf "providers.%s: apiKey.existingSecret.name and .key are required (API keys are never read from values)" $name) -}}
{{- end -}}
{{- $_ := set $entry "secretName" $sec.name -}}
{{- $_ := set $entry "secretKey" $sec.key -}}
{{- $_ := set $entry "envName" (printf "PROVIDER_%s_API_KEY" (upper (replace "-" "_" $name))) -}}
{{- else -}}
{{- fail (printf "providers.%s: unsupported type %q (valid types: claude, anthropic-api, openai-api; codex is planned, not implemented)" $name $type) -}}
{{- end -}}
{{- $out = append $out $entry -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}
