# Getting Started

**LLM Proxy ManifestD** sits in front of an OpenAI-compatible endpoint (for example
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) or an upstream API) and makes sure that
a shared account never receives more parallel requests than you allow. Excess requests wait in a
priority queue.

::: tip Kubernetes-first
This project targets Kubernetes: it is deployed with the Helm chart and configured through a ConfigMap that hot-reloads. Optional CLI sidecars for Claude subscription logins run in the same pod via the chart's `providers:` value (see [Subscription providers](/guide/subscription-providers)); the adapter is experimental and has never run against a real login. A Codex sidecar is planned and not implemented. Sidecar-less API providers (`anthropic-api`, `openai-api`) also exist. The binary is a static Go program that reads a config file, so it can run under Docker or on a laptop too, but Kubernetes is the primary, supported target.
:::

## Install

Create a secret holding the credentials referenced from the manifest as `${VAR}`:

```sh
kubectl create secret generic llm-proxy-keys \
  --from-literal=LOCAL_BRIDGE_KEY=change-me
```

Write a `values.yaml`:

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
  endpoints:
    openai:
      codex-interactive: {priority: 100, targets: [{pool: codex-sub, target_model: gpt-5.4}]}
      codex-default:     {priority: 50,  targets: [{pool: codex-sub, target_model: gpt-5.4}]}
      codex-background:  {priority: 10,  targets: [{pool: codex-sub, target_model: gpt-5.4}]}
```

Install or upgrade:

```sh
helm upgrade --install llm-proxy oci://ghcr.io/lerenn/charts/llm-proxy-manifestd \
  -f values.yaml
```

Config changes are hot-reloaded: `helm upgrade` updates the ConfigMap and the running pod picks it up
(Kubernetes propagates ConfigMap changes within about a minute) without restarting.

## Use

Point any OpenAI-compatible client at the service and use an **endpoint name** as the model:

```sh
curl http://llm-proxy/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"codex-interactive","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

The proxy picks one of the endpoint's targets ([multi-pool routing](./multi-pool-routing)), rewrites
`model` to that target's `target_model`, takes a slot from its pool, injects the pool's API key and
streams the response back. `/v1/messages` resolves names among `endpoints.anthropic`, every other
`/v1/*` path (`/v1/chat/completions`, `/v1/responses`, ...) among `endpoints.openai`
([protocol endpoints](./protocol-endpoints)).

## Endpoints

| Endpoint | Description |
| --- | --- |
| `GET /v1/models` | Lists the endpoint names of the caller's protocol (Anthropic shape with an `anthropic-version` or `x-api-key` header, OpenAI shape otherwise). |
| `GET /status` | Per pool: `max_concurrency`, `active_slots`, `queue_depth`, `total_served`. |
| `GET /healthz` | `200 {"status":"ok"}` once a valid config is loaded. |

## Response headers

| Header | Meaning |
| --- | --- |
| `X-Queue-Wait-Ms` | Time spent waiting for a slot. |
| `X-Pool-Name` | Pool that served the request. |

Send `X-Priority: <int>` to override the endpoint priority for a single request.

## Errors

Errors use the OpenAI format (`{"error": {"message", "type", "param", "code"}}`), or the Anthropic one on `/v1/messages`:

| Status | Code | Cause |
| --- | --- | --- |
| 400 | `missing_model`, `invalid_json`, `invalid_priority` | Malformed request. |
| 404 | `endpoint_not_configured` | No endpoint with that name for the request's protocol. |
| 503 | `queue_timeout` | No slot became free within `server.queue_timeout`. |
| 502 | `upstream_error` | The upstream could not be reached. |
