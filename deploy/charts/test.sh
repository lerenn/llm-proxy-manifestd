#!/usr/bin/env bash
# Helm template tests for the llm-proxy-manifestd chart (provider type `api`).
# Usage: deploy/charts/test.sh   (needs helm and go; run from anywhere)
set -euo pipefail
cd "$(dirname "$0")/../.."
CHART=deploy/charts/llm-proxy-manifestd
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
fails=0

render() { helm template r "$CHART" -f "$1" -s templates/configmap.yaml; }
ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails+1)); }
secret='apiKey: {existingSecret: {name: s, key: k}}'

# expect_in <name> <values> <grep -E pattern>...  (all patterns must match the rendered ConfigMap)
expect_in() {
  local n=$1 f=$2; shift 2
  local out; out=$(render "$f" 2>&1) || { fail "$n (render failed: $out)"; return; }
  for p in "$@"; do grep -qE "$p" <<<"$out" || { fail "$n (missing: $p)"; return; }; done
  ok "$n"
}
# expect_err <name> <values> <message fragment>
expect_err() {
  local out; out=$(render "$2" 2>&1) && { fail "$1 (should fail)"; return; }
  grep -qF "$3" <<<"$out" && ok "$1" || fail "$1 (wrong error: $out)"
}
v() { printf '%s\n' "$2" > "$tmp/$1.yaml"; echo "$tmp/$1.yaml"; }

expect_in legacy-openai-api "$(v a "providers: {oa: {type: openai-api, maxConcurrency: 2, $secret}}")" \
  '^      oa:' 'protocol: openai' 'upstream_url: https://api.openai.com'
render "$(v a2 "providers: {oa: {type: openai-api, $secret}}")" | grep -qE 'auth:|concurrency_group' && fail "legacy: no auth/group expected" || ok "legacy-no-auth-no-group"
expect_in legacy-anthropic-api "$(v b "providers: {an: {type: anthropic-api, $secret}}")" \
  '^      an:' 'protocol: anthropic' 'upstream_url: https://api.anthropic.com'
expect_in api-format-string "$(v c "providers: {nm: {type: api, format: openai, url: 'https://x.example', auth: bearer, maxConcurrency: 1, $secret}}")" \
  '^      nm:' 'auth: bearer' 'protocol: openai' 'upstream_url: https://x.example'
expect_in api-format-list "$(v d "providers: {nm: {type: api, format: [anthropic, openai], url: 'https://x.example', auth: bearer, maxConcurrency: 1, $secret}}")" \
  '^      nm-anthropic:' '^      nm-openai:' 'concurrency_group: nm' 'auth: bearer'
expect_err api-missing-format "$(v e "providers: {nm: {type: api, url: 'https://x', $secret}}")" 'requires `format`'
expect_err api-missing-url "$(v f "providers: {nm: {type: api, format: openai, $secret}}")" 'requires `url`'
expect_err api-bad-format "$(v g "providers: {nm: {type: api, format: [gemini], url: 'https://x', $secret}}")" 'invalid format'
expect_err api-dup-format "$(v h "providers: {nm: {type: api, format: [openai, openai], url: 'https://x', $secret}}")" 'duplicate'
expect_err api-bad-auth "$(v i "providers: {nm: {type: api, format: openai, url: 'https://x', auth: basic, $secret}}")" 'invalid auth'
expect_err api-no-secret "$(v j "providers: {nm: {type: api, format: openai, url: 'https://x'}}")" 'apiKey.existingSecret'
expect_err format-on-legacy "$(v k "providers: {nm: {type: openai-api, format: openai, $secret}}")" 'only valid for type api'

# The rendered config must pass the checker.
f=$(v l "providers: {nm: {type: api, format: [anthropic, openai], url: 'https://x.example', auth: bearer, maxConcurrency: 1, $secret}}
config: {endpoints: {anthropic: {fast: {targets: [{pool: nm-anthropic, target_model: m}]}}, openai: {fast: {targets: [{pool: nm-openai, target_model: m}]}}}}")
helm template r "$CHART" -f "$f" -s templates/configmap.yaml | sed -n '/config.yaml: |/,$p' | sed '1d;s/^    //' > "$tmp/config.yaml"
PROVIDER_NM_API_KEY=x go run ./cmd/checker -f "$tmp/config.yaml" | grep -q '1 concurrency groups' && ok checker-accepts-rendered || fail checker-accepts-rendered

[ "$fails" -eq 0 ] || { echo "$fails failure(s)"; exit 1; }
