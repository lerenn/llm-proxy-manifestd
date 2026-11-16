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
Providers: entries of .Values.providers. Types claude / codex = CLI adapter sidecar (+PVC);
types api / anthropic-api / openai-api = pool(s) only (no container).
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
{{- if lt (int ($a.maxConcurrency | default 0)) 0 -}}
{{- fail (printf "providers.%s: maxConcurrency must be >= 0 (0 or omitted = unlimited)" $name) -}}
{{- end -}}
{{- $entry := dict "name" $name "type" $type "maxConcurrency" (int ($a.maxConcurrency | default 0)) "provider" $a -}}
{{- if or (eq $type "claude") (eq $type "codex") -}}
{{- $_ := set $entry "sidecar" true -}}
{{- $_ := set $entry "protocol" (ternary "anthropic" "openai" (eq $type "claude")) -}}
{{- $_ := set $entry "homeEnv" (ternary "CLAUDE_CONFIG_DIR" "CODEX_HOME" (eq $type "claude")) -}}
{{- $_ := set $entry "dataDir" (printf "/data/%s" $type) -}}
{{- $_ := set $entry "port" (int ($a.port | default (add $base $sidecars))) -}}
{{- $sidecars = add1 $sidecars -}}
{{- else if or (eq $type "anthropic-api") (eq $type "openai-api") (eq $type "api") -}}
{{- $_ := set $entry "sidecar" false -}}
{{- $formats := list -}}
{{- $url := "" -}}
{{- if eq $type "api" -}}
{{- $fmt := $a.format | default "" -}}
{{- if kindIs "string" $fmt -}}
{{- $formats = ternary (list) (list $fmt) (eq $fmt "") -}}
{{- else if kindIs "slice" $fmt -}}
{{- $formats = $fmt -}}
{{- else -}}
{{- fail (printf "providers.%s: format must be a string or a list (anthropic, openai)" $name) -}}
{{- end -}}
{{- if not $formats -}}
{{- fail (printf "providers.%s: type api requires `format`: anthropic, openai or [anthropic, openai]" $name) -}}
{{- end -}}
{{- range $f := $formats -}}
{{- if not (has $f (list "anthropic" "openai")) -}}
{{- fail (printf "providers.%s: invalid format %v (allowed: anthropic, openai)" $name $f) -}}
{{- end -}}
{{- end -}}
{{- if ne (len $formats) (len (uniq $formats)) -}}
{{- fail (printf "providers.%s: duplicate entries in format" $name) -}}
{{- end -}}
{{- if not $a.url -}}
{{- fail (printf "providers.%s: type api requires `url` (e.g. https://api.example.com; no trailing /v1)" $name) -}}
{{- end -}}
{{- $url = $a.url -}}
{{- else -}}
{{- if $a.format -}}
{{- fail (printf "providers.%s: `format` is only valid for type api" $name) -}}
{{- end -}}
{{- $formats = list (ternary "anthropic" "openai" (eq $type "anthropic-api")) -}}
{{- $url = $a.url | default (ternary "https://api.anthropic.com" "https://api.openai.com" (eq $type "anthropic-api")) -}}
{{- end -}}
{{- if and $a.auth (not (has $a.auth (list "bearer" "x-api-key"))) -}}
{{- fail (printf "providers.%s: invalid auth %v (allowed: bearer, x-api-key)" $name $a.auth) -}}
{{- end -}}
{{- $apiPools := list -}}
{{- range $f := $formats -}}
{{- $pn := ternary (printf "%s-%s" $name $f) $name (gt (len $formats) 1) -}}
{{- $apiPools = append $apiPools (dict "name" $pn "protocol" $f) -}}
{{- end -}}
{{- $_ := set $entry "apiPools" $apiPools -}}
{{- $_ := set $entry "group" (ternary $name "" (gt (len $formats) 1)) -}}
{{- $_ := set $entry "auth" ($a.auth | default "") -}}
{{- $_ := set $entry "url" $url -}}
{{- $sec := dig "apiKey" "existingSecret" dict $a -}}
{{- if not (and $sec.name $sec.key) -}}
{{- fail (printf "providers.%s: apiKey.existingSecret.name and .key are required (API keys are never read from values)" $name) -}}
{{- end -}}
{{- $_ := set $entry "secretName" $sec.name -}}
{{- $_ := set $entry "secretKey" $sec.key -}}
{{- $_ := set $entry "envName" (printf "PROVIDER_%s_API_KEY" (upper (replace "-" "_" $name))) -}}
{{- else -}}
{{- fail (printf "providers.%s: unsupported type %q (valid types: claude, codex, api, anthropic-api, openai-api)" $name $type) -}}
{{- end -}}
{{- $out = append $out $entry -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{/* Client API keys: enabled when config.clients has at least one entry. */}}
{{- define "llm-proxy-manifestd.clientKeysEnabled" -}}
{{- if .Values.config.clients -}}true{{- end -}}
{{- end -}}

{{- define "llm-proxy-manifestd.clientKeysSecret" -}}
{{- .Values.clientsExistingSecret | default (printf "%s-client-keys" (include "llm-proxy-manifestd.fullname" .)) -}}
{{- end -}}

{{- define "llm-proxy-manifestd.clientKeysPath" -}}
/etc/llm-proxy-manifestd-client-keys
{{- end -}}

{{/* The chart creates and maintains the client keys Secret (init container + RBAC). */}}
{{- define "llm-proxy-manifestd.keysManager" -}}
{{- if and (include "llm-proxy-manifestd.clientKeysEnabled" .) (not .Values.clientsExistingSecret) -}}true{{- end -}}
{{- end -}}
