# llm-proxy-manifestd

Headless, IaC-first Go LLM reverse proxy for Kubernetes with priority queuing, per-pool concurrency limits, SSE streaming, and ConfigMap hot-reloading—a public read-only status dashboard embedded in the binary, zero database.

Everything is declared in a single YAML manifest delivered through Helm. There is nothing to click, nothing to migrate, and no state to back up: change your values, run `helm upgrade`, and the running proxy reloads the new manifest without dropping a single in-flight stream.

## Install

Create a secret with the credentials referenced as `${VAR}` in the manifest:

```sh
kubectl create secret generic llm-proxy-keys \
  --from-literal=LOCAL_BRIDGE_KEY=change-me
```

Describe your pools and routes in `values.yaml`:

```yaml
existingSecret: llm-proxy-keys

config:
  server:
    queue_timeout: "120s"
    aging_interval: "5s"
  pools:
    codex-sub:
      upstream_url: http://cliproxyapi.default.svc.cluster.local:8317
      api_key: "${LOCAL_BRIDGE_KEY}"
      max_concurrency: 2
  routes:
    codex-interactive: {pool: codex-sub, target_model: gpt-5.4, priority: 100}
    codex-default:     {pool: codex-sub, target_model: gpt-5.4, priority: 50}
    codex-background:  {pool: codex-sub, target_model: gpt-5.4, priority: 10}
```

Install, then upgrade whenever the manifest changes:

```sh
helm install llm-proxy oci://ghcr.io/lerenn/charts/llm-proxy-manifestd -f values.yaml
helm upgrade llm-proxy oci://ghcr.io/lerenn/charts/llm-proxy-manifestd -f values.yaml
```

Clients then talk OpenAI-compatible HTTP to the service and use a route name (e.g. `codex-interactive`) as the model.

## Dashboard

The binary embeds a lightweight, read-only dashboard (status, version, uptime, slots, queue depth per priority, per-route in-flight/queued/requests/errors, last config reload). It is public and shows only names, upstream host names, counts and timings, never keys, URLs, prompts or headers.

It is served at `/` for hosts starting with `dashboard.` (and only the dashboard is served there, never `/v1/...`) and under `/dashboard/` on every host. Expose it on its own host with the chart:

```yaml
dashboard:
  ingress:
    enabled: true
    host: dashboard.llm-proxy.example.com
```

See the [Dashboard guide](https://llm-proxy-manifestd.lerenn.dev/guide/dashboard) for the `dashboard:` manifest keys.

## Documentation

Full documentation (architecture, priority queues, Helm values) is available at <https://llm-proxy-manifestd.lerenn.dev> and from <https://lerenn.dev>.

## License

MIT
