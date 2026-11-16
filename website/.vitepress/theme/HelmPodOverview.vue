<script setup lang="ts">
// Edit this block to adjust the displayed values (real chart schema, see deploy/charts/.../values.yaml).
const values = `providers:
  claude-subscription:
    type: claude
    maxConcurrency: 4
config:
  endpoints:
    anthropic:
      standard-medium:
        priority: 50
        targets:
          - pool: claude-subscription
            target_model: "claude-sonnet-5-5@medium"
      premium-high:
        priority: 80
        targets:
          - pool: claude-subscription
            target_model: "claude-opus-5-5@high"
dashboard:
  ingress:
    enabled: true
    host: dashboard.llm-proxy.example.com`
</script>

<template>
  <section class="hpo" aria-label="Helm values and resulting pod">
    <div class="hpo-grid">
      <div class="hpo-col">
        <h3 class="hpo-title">values.yaml</h3>
        <pre class="hpo-code"><code>{{ values }}</code></pre>
      </div>
      <div class="hpo-col">
        <h3 class="hpo-title">Resulting pod</h3>
        <div class="hpo-diagram" role="img"
          aria-label="Clients call the proxy container on port 8080. The proxy calls vendor APIs directly (API passthrough) or through CLI sidecars on localhost for subscription logins.">
          <div class="hpo-row hpo-ext">Clients (coding agents)</div>
          <div class="hpo-arrow">&darr; <small>:8080</small></div>
          <div class="hpo-pod">
            <span class="hpo-pod-label">Pod</span>
            <div class="hpo-box hpo-proxy">
              <b>llm-proxy-manifestd</b>
              <small>:8080 &middot; dashboard &middot; queues</small>
              <div class="hpo-cm">ConfigMap (endpoints) mounted</div>
            </div>
            <div class="hpo-arrow">&darr; <small>localhost</small></div>
            <div class="hpo-sidecars">
              <div class="hpo-box">
                <b>API passthrough</b>
                <small>no sidecar &middot; direct to the vendor API with an API key</small>
                <small>types: anthropic-api / openai-api</small>
              </div>
              <div class="hpo-box">
                <b>CLI sidecar <span class="hpo-sub">(for subscriptions)</span></b>
                <small>one per subscription login &middot; 127.0.0.1:&lt;port&gt; &middot; login on PVC</small>
                <small>types: claude / codex</small>
              </div>
            </div>
          </div>
          <div class="hpo-arrow">&darr;</div>
          <div class="hpo-row hpo-ext">Vendor APIs (Anthropic, OpenAI)</div>
        </div>
      </div>
    </div>
    <p class="hpo-caption">Providers are either API passthrough pools or CLI sidecars for subscription logins. See <a href="/guide/subscription-providers">Subscription providers</a>.</p>
  </section>
</template>

<style scoped>
.hpo { box-sizing: border-box; margin: 0; padding: 8px 24px 32px; width: 100%; text-align: left; }
.hpo > * { max-width: 1152px; margin-left: auto; margin-right: auto; }
/* Same horizontal padding as VitePress's feature cards (.VPFeatures) so both blocks line up. */
@media (min-width: 640px) { .hpo { padding-left: 48px; padding-right: 48px; } }
@media (min-width: 960px) { .hpo { padding-left: 64px; padding-right: 64px; } }
.hpo-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; align-items: start; }
@media (max-width: 768px) { .hpo-grid { grid-template-columns: 1fr; } }
.hpo-col { min-width: 0; }
.hpo-title { margin: 0 0 8px; font-size: 14px; font-weight: 600; color: var(--vp-c-text-2); }
.hpo-code { margin: 0; padding: 14px 16px; overflow-x: auto; font-size: 12.5px; line-height: 1.55;
  background: var(--vp-c-bg-soft); border: 1px solid var(--vp-c-divider); border-radius: 8px;
  font-family: var(--vp-font-family-mono); color: var(--vp-c-text-1); }
.hpo-diagram { display: flex; flex-direction: column; align-items: stretch; font-size: 13px; }
.hpo-row, .hpo-box, .hpo-pod { border: 1px solid var(--vp-c-divider); border-radius: 8px; }
.hpo-ext { padding: 8px; text-align: center; background: var(--vp-c-bg-soft); color: var(--vp-c-text-2); }
.hpo-arrow { text-align: center; color: var(--vp-c-brand-1); line-height: 1.6; }
.hpo-arrow small { color: var(--vp-c-text-3); }
.hpo-pod { position: relative; padding: 26px 12px 12px; border: 2px dashed var(--vp-c-brand-1); }
.hpo-pod-label { position: absolute; top: 4px; left: 10px; font-size: 11px; font-weight: 600;
  text-transform: uppercase; color: var(--vp-c-brand-1); }
.hpo-box { display: flex; flex-direction: column; gap: 2px; padding: 8px 10px; background: var(--vp-c-bg); }
.hpo-box small { color: var(--vp-c-text-2); font-size: 11.5px; overflow-wrap: anywhere; }
.hpo-proxy { border-color: var(--vp-c-brand-1); }
.hpo-cm { margin-top: 6px; padding: 4px 8px; font-size: 11.5px; border-radius: 6px;
  background: var(--vp-c-brand-soft); color: var(--vp-c-text-1); }
.hpo-sidecars { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
@media (max-width: 480px) { .hpo-sidecars { grid-template-columns: 1fr; } }
.hpo-sub { font-weight: 400; color: var(--vp-c-text-2); }
.hpo > .hpo-caption { margin-top: 12px; margin-bottom: 0; font-size: 12.5px; color: var(--vp-c-text-3); text-align: center; }
</style>
