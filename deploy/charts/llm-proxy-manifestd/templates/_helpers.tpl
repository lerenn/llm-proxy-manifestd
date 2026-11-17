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
Providers: entries of .Values.providers. Type codex = CLI adapter sidecar (+PVC);
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
{{- if eq $type "codex" -}}
{{- $_ := set $entry "sidecar" true -}}
{{- $_ := set $entry "protocol" "openai" -}}
{{- $_ := set $entry "homeEnv" "CODEX_HOME" -}}
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
{{- fail (printf "providers.%s: unsupported type %q (valid types: codex, api, anthropic-api, openai-api)" $name $type) -}}
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
{{- define "llm-proxy-manifestd.clientKeysManager" -}}
{{- if and (include "llm-proxy-manifestd.clientKeysEnabled" .) (not .Values.clientsExistingSecret) -}}true{{- end -}}
{{- end -}}

{{/* adapters.mode, validated. */}}
{{- define "llm-proxy-manifestd.adaptersMode" -}}
{{- $m := (.Values.adapters).mode | default "sidecar" -}}
{{- if not (has $m (list "sidecar" "separate")) -}}
{{- fail (printf "adapters.mode must be sidecar or separate (got %q)" $m) -}}
{{- end -}}
{{- $m -}}
{{- end -}}

{{/* "true" when subscription providers run in their own Deployments (adapters.mode=separate and at least one codex provider). */}}
{{- define "llm-proxy-manifestd.separate" -}}
{{- if eq (include "llm-proxy-manifestd.adaptersMode" .) "separate" -}}
{{- range (include "llm-proxy-manifestd.providers" . | fromJsonArray) -}}{{- if .sidecar -}}true{{- end -}}{{- end -}}
{{- end -}}
{{- end -}}

{{/* "true" when adapter sidecars live in the proxy pod (adapters.mode=sidecar and at least one codex provider). */}}
{{- define "llm-proxy-manifestd.inPodAdapters" -}}
{{- if eq (include "llm-proxy-manifestd.adaptersMode" .) "sidecar" -}}
{{- range (include "llm-proxy-manifestd.providers" . | fromJsonArray) -}}{{- if .sidecar -}}true{{- end -}}{{- end -}}
{{- end -}}
{{- end -}}

{{/* Shared limiter/queue backend configured: the chart's Valkey or a user-set config.coordination. */}}
{{- define "llm-proxy-manifestd.coordination" -}}
{{- if or (.Values.valkey).enabled .Values.config.coordination -}}true{{- end -}}
{{- end -}}

{{/*
Proxy rollout shape (see values.yaml "Rolling updates"): "rolling" only when adapters are separate AND
a shared limiter is configured; "single" (replicas 1, Recreate) when adapters live in the pod, or in separate
mode without a shared limiter; "plain" (replicaCount + RollingUpdate, as before) with no subscription provider.
*/}}
{{- define "llm-proxy-manifestd.rollout" -}}
{{- if include "llm-proxy-manifestd.inPodAdapters" . -}}single
{{- else if include "llm-proxy-manifestd.separate" . -}}{{ ternary "rolling" "single" (not (not (include "llm-proxy-manifestd.coordination" .))) }}
{{- else -}}plain{{- end -}}
{{- end -}}

{{- define "llm-proxy-manifestd.adapterTokenSecret" -}}
{{ include "llm-proxy-manifestd.fullname" . }}-adapter-token
{{- end -}}

{{- define "llm-proxy-manifestd.valkeyName" -}}
{{ include "llm-proxy-manifestd.fullname" . }}-valkey
{{- end -}}

{{/* keys-manager init container: client keys and/or adapter token. */}}
{{- define "llm-proxy-manifestd.keysManager" -}}
{{- if or (include "llm-proxy-manifestd.clientKeysManager" .) (include "llm-proxy-manifestd.separate" .) -}}true{{- end -}}
{{- end -}}
