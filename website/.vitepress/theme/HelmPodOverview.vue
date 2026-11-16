<script setup lang="ts">
// Edit this block to adjust the displayed values (real chart schema, see deploy/charts/.../values.yaml; `providers` replaces `accounts` once the rename lands).
const values = `providers:
  claude-subscription:
    type: claude
    maxConcurrency: 4
config:
  routes:
    standard-medium:
      endpoints:
        anthropic:
          pools: [claude-subscription]
          target_model: "claude-sonnet-5-5@medium"
          priority: 50
    premium-high:
      endpoints:
        anthropic:
          pools: [claude-subscription]
          target_model: "claude-opus-5-5@high"
          priority: 80
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
          aria-label="Clients call the proxy container on port 8080. The proxy forwards to the claude sidecar on localhost (a codex sidecar is planned), which calls the vendor API.">
          <div class="hpo-row hpo-ext">Clients (coding agents)</div>
          <div class="hpo-arrow">&darr; <small>:8080</small></div>
          <div class="hpo-pod">
            <span class="hpo-pod-label">Pod</span>
            <div class="hpo-box hpo-proxy">
              <b>llm-proxy-manifestd</b>
              <small>:8080 &middot; dashboard &middot; queues</small>
              <div class="hpo-cm">ConfigMap (routes) mounted</div>
            </div>
            <div class="hpo-arrow">&darr; <small>localhost</small></div>
            <div class="hpo-sidecars">
              <div class="hpo-box">
                <b>claude-subscription</b>
                <small>provider sidecar &middot; 127.0.0.1:9100</small>
                <small>login on PVC</small>
              </div>
              <div class="hpo-box hpo-exp">
                <b>codex provider sidecar</b>
                <small>not implemented</small>
                <em>planned</em>
              </div>
            </div>
          </div>
          <div class="hpo-arrow">&darr;</div>
          <div class="hpo-row hpo-ext">Vendor APIs (Anthropic, OpenAI)</div>
        </div>
      </div>
    </div>
    <p class="hpo-caption">Provider sidecar layout available for Claude; Codex provider is planned. See <a href="/guide/subscription-accounts">Subscription accounts</a>.</p>
  </section>
</template>

<style scoped>
.hpo { margin-top: 32px; width: 100%; max-width: 960px; text-align: left; }
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
.hpo-exp { border-style: dashed; }
.hpo-exp em { font-size: 11.5px; font-style: normal; color: var(--vp-c-warning-1); }
.hpo-caption { margin: 12px 0 0; font-size: 12.5px; color: var(--vp-c-text-3); text-align: center; }
</style>
