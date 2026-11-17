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

# --- client keys: keys-manager init container + RBAC ---
full() { helm template r "$CHART" -f "$1" 2>&1; }
cl=$(v m "config: {clients: {a: {}, b: {}}}")
out=$(full "$cl") || fail "clients render"
init=$(awk '/initContainers:/{f=1} /^      containers:/{f=0} f' <<<"$out")
grep -q 'name: keys-manager' <<<"$out" && grep -q 'command: \["/keys-manager"\]' <<<"$out" && grep -q -- '- init' <<<"$out" && ok keysmgr-init-present || fail keysmgr-init-present
grep -q 'kind: Role$' <<<"$out" && grep -q 'kind: RoleBinding' <<<"$out" && grep -q 'kind: ServiceAccount' <<<"$out" && ok keysmgr-rbac || fail keysmgr-rbac
grep -qE 'resourceNames: \["r-llm-proxy-manifestd-client-keys"\]' <<<"$out" && grep -q 'verbs: \["create"\]' <<<"$out" && ok keysmgr-rbac-scoped || fail keysmgr-rbac-scoped
! grep -q 'kind: Secret' <<<"$out" && ok no-lookup-secret || fail no-lookup-secret
[ ! -e "$CHART/templates/client-keys-secret.yaml" ] && ! grep -rq 'lookup' "$CHART/templates" && ok lookup-template-gone || fail lookup-template-gone
# token only in the init container
[ "$(grep -c 'kube-api-access' <<<"$out")" -eq 2 ] && grep -q 'automountServiceAccountToken: false' <<<"$out" && ok token-only-in-init || fail token-only-in-init
sec=$(awk '/- name: keys-manager/{f=1} /- name: llm-proxy-manifestd$/{f=0} f' <<<"$out")
for p in 'readOnlyRootFilesystem: true' 'runAsNonRoot: true' 'drop: \["ALL"\]'; do grep -qE "$p" <<<"$sec" || fail "keysmgr-hardening ($p)"; done; ok keysmgr-hardening
# token stays out of the main container with sidecar providers
out2=$(full "$(v n "config: {clients: {a: {}}}
providers: {c: {type: codex}}")") && [ "$(grep -c 'kube-api-access' <<<"$out2")" -eq 2 ] && grep -q 'automountServiceAccountToken: false' <<<"$out2" && ok token-only-in-init-sidecars || fail token-only-in-init-sidecars
# escape hatch
out3=$(full "$(v o "clientsExistingSecret: mine
config: {clients: {a: {}}}")")
! grep -qE 'keys-manager|kind: Role|kind: ServiceAccount|kube-api-access' <<<"$out3" && grep -q 'secretName: mine' <<<"$out3" && ok existing-secret-no-init || fail existing-secret-no-init
# volume optional only for the generated Secret
grep -A4 'secretName: r-llm-proxy-manifestd-client-keys' <<<"$out" | grep -q 'optional: true' && ok keys-volume-optional-generated || fail keys-volume-optional-generated
! grep -A4 'secretName: mine' <<<"$out3" | grep -q 'optional' && ok keys-volume-required-existing || fail keys-volume-required-existing
# adapters never gate readiness; proxy keeps its probe
out5=$(full "$(v q "providers: {c: {type: codex}}")")
[ "$(grep -c '^          readinessProbe:' <<<"$out5")" -eq 1 ] && grep -q 'ADAPTER_TYPE' <<<"$out5" && ok adapters-no-readiness || fail adapters-no-readiness
# no clients: nothing
out4=$(full "$(v p "{}")")
! grep -qE 'keys-manager|kind: Role|client-keys' <<<"$out4" && ok no-clients-no-init || fail no-clients-no-init

# --- adapters.mode=separate + valkey (rolling updates) ---
sep="adapters: {mode: separate}
providers: {cl: {type: codex, maxConcurrency: 2}, cx: {type: codex}}"
vk="valkey: {enabled: true}"
outS=$(full "$(v s1 "$sep
$vk
replicaCount: 2
config: {clients: {a: {}}}")") || fail "separate render"
nd=$(grep -c '^kind: Deployment' <<<"$outS"); [ "$nd" -eq 4 ] && ok separate-deployments || fail "separate-deployments ($nd)"
for n in r-llm-proxy-manifestd-cl r-llm-proxy-manifestd-cx r-llm-proxy-manifestd-valkey; do grep -q "^  name: $n\$" <<<"$outS" || fail "separate-object $n"; done; ok separate-objects
proxyDep=$(awk 'BEGIN{RS="---\n"} /kind: Deployment/ && /name: r-llm-proxy-manifestd\n/ {print}' <<<"$outS")
grep -q 'type: RollingUpdate' <<<"$proxyDep" && grep -q 'maxSurge: 1' <<<"$proxyDep" && grep -q 'maxUnavailable: 0' <<<"$proxyDep" && grep -q 'replicas: 2' <<<"$proxyDep" && ok rolling-update-conditions || fail rolling-update-conditions
! grep -qE 'persistentVolumeClaim|name: ADAPTER_TYPE|name: cl$' <<<"$proxyDep" && ok proxy-no-pvc-no-sidecar || fail proxy-no-pvc-no-sidecar
grep -q 'name: ADAPTER_TOKEN' <<<"$proxyDep" && grep -q 'adapter-token' <<<"$proxyDep" && grep -q -- '--adapter-token-secret=r-llm-proxy-manifestd-adapter-token' <<<"$proxyDep" && ok token-wired-proxy || fail token-wired-proxy
adDep=$(awk 'BEGIN{RS="---\n"} /kind: Deployment/ && /name: r-llm-proxy-manifestd-cl\n/ {print}' <<<"$outS")
grep -q 'type: Recreate' <<<"$adDep" && grep -q 'replicas: 1' <<<"$adDep" && grep -q 'name: ADAPTER_TOKEN' <<<"$adDep" && grep -q 'ADAPTER_LISTEN' <<<"$adDep" && grep -q 'readOnlyRootFilesystem: true' <<<"$adDep" && ! grep -q '^ *readinessProbe' <<<"$adDep" && ok adapter-deployment || fail adapter-deployment
grep -q 'name: ADAPTER_SHUTDOWN_TIMEOUT' <<<"$adDep" && grep -q 'value: "595s"' <<<"$adDep" && grep -q 'sleep' <<<"$adDep" && ok adapter-graceful-shutdown || fail adapter-graceful-shutdown
grep -qE 'upstream_url: http://r-llm-proxy-manifestd-cl:9100' <<<"$outS" && grep -qE 'upstream_url: http://r-llm-proxy-manifestd-cx:9101' <<<"$outS" && grep -q 'api_key: ${ADAPTER_TOKEN}' <<<"$outS" && ok separate-pools || fail separate-pools
grep -q 'address: r-llm-proxy-manifestd-valkey:6379' <<<"$outS" && ok valkey-coordination-injected || fail valkey-coordination-injected
grep -qE 'resourceNames: \["r-llm-proxy-manifestd-client-keys", "r-llm-proxy-manifestd-adapter-token"\]' <<<"$outS" && ok token-rbac || fail token-rbac
# adapter pods must never match the proxy selector
! grep -A3 'selector:' <<<"$adDep" | grep -q 'name: llm-proxy-manifestd$' && ok adapter-labels-distinct || fail adapter-labels-distinct
# valkey password: env + requirepass from env, never argv; proxy gets VALKEY_PASSWORD
outV=$(full "$(v s2 "$vk
valkey: {enabled: true, existingSecret: {name: vs, key: pw}}")")
grep -q 'password_env: VALKEY_PASSWORD' <<<"$outV" && grep -q 'name: VALKEY_PASSWORD' <<<"$outV" && grep -q -- '--requirepass \\"$VALKEY_PASSWORD\\"' <<<"$outV" && ok valkey-password || fail valkey-password
grep -q 'image: "valkey/valkey:8' <<<"$outV" && ! grep -q 'valkey/valkey:latest' <<<"$outV" && ok valkey-pinned || fail valkey-pinned
# user-set config.coordination wins over the injected one
outC=$(full "$(v s3 "$vk
config: {coordination: {valkey: {address: 'my:6379'}}}")")
grep -q 'address: my:6379' <<<"$outC" && ! grep -q -- '-valkey:6379' <<<"$outC" && ok coordination-user-wins || fail coordination-user-wins
# separate without a shared limiter: single replica, Recreate
outN=$(full "$(v s4 "$sep")"); pN=$(awk 'BEGIN{RS="---\n"} /kind: Deployment/ && /name: r-llm-proxy-manifestd\n/ {print}' <<<"$outN")
grep -q 'type: Recreate' <<<"$pN" && grep -q 'replicas: 1' <<<"$pN" && ok separate-no-limiter-recreate || fail separate-no-limiter-recreate
# config.coordination alone enables rolling
outR=$(full "$(v s5 "$sep
config: {coordination: {valkey: {address: 'my:6379'}}}")"); pR=$(awk 'BEGIN{RS="---\n"} /kind: Deployment/ && /name: r-llm-proxy-manifestd\n/ {print}' <<<"$outR")
grep -q 'type: RollingUpdate' <<<"$pR" && ok coordination-rolling || fail coordination-rolling
# sidecar mode untouched
outSC=$(full "$(v s6 "providers: {cl: {type: codex}}")"); pSC=$(awk 'BEGIN{RS="---\n"} /kind: Deployment/ && /name: r-llm-proxy-manifestd\n/ {print}' <<<"$outSC")
grep -q 'type: Recreate' <<<"$pSC" && grep -q 'ADAPTER_LISTEN' <<<"$pSC" && grep -q '127.0.0.1:9100' <<<"$pSC" && ! grep -q 'adapter-token\|ADAPTER_TOKEN' <<<"$outSC" && ok sidecar-mode-unchanged || fail sidecar-mode-unchanged
# render errors
errc() { local o; o=$(full "$2") && { fail "$1 (should fail)"; return; }; grep -qF "$3" <<<"$o" && ok "$1" || fail "$1 (wrong error: $o)"; }
errc bad-mode "$(v e1 "adapters: {mode: nope}")" 'adapters.mode must be'
errc replicas-sidecar "$(v e2 "replicaCount: 2
providers: {cl: {type: codex}}")" 'replicaCount > 1'
errc replicas-no-limiter "$(v e3 "replicaCount: 2
$sep")" 'replicaCount > 1'


[ "$fails" -eq 0 ] || { echo "$fails failure(s)"; exit 1; }
