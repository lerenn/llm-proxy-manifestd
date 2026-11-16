# Getting Started

`llm-proxy-manifestd` sits in front of an OpenAI-compatible endpoint (for example
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) or an upstream API) and makes sure that
a shared account never receives more parallel requests than you allow. Excess requests wait in a
priority queue.

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
  routes:
    codex-interactive: {pool: codex-sub, target_model: gpt-5.4, priority: 100}
    codex-default:     {pool: codex-sub, target_model: gpt-5.4, priority: 50}
    codex-background:  {pool: codex-sub, target_model: gpt-5.4, priority: 10}
```

Install or upgrade:

```sh
helm upgrade --install llm-proxy oci://ghcr.io/lerenn/charts/llm-proxy-manifestd \
  -f values.yaml
```

Config changes are hot-reloaded: `helm upgrade` updates the ConfigMap and the running pod picks it up
(Kubernetes propagates ConfigMap changes within about a minute) without restarting.

## Use

Point any OpenAI-compatible client at the service and use a **route name** as the model:

```sh
curl http://llm-proxy/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"codex-interactive","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

The proxy rewrites `model` to the route's `target_model`, takes a slot from the route's pool, injects
the pool's API key and streams the response back. `/v1/chat/completions`, `/v1/responses` and
`/v1/messages` all work: every `/v1/*` request carrying a JSON `model` is routed.

## Endpoints

| Endpoint | Description |
| --- | --- |
| `GET /v1/models` | Lists the route names as models. |
| `GET /status` | Per pool: `max_concurrency`, `active_slots`, `queue_depth`, `total_served`. |
| `GET /healthz` | `200 {"status":"ok"}` once a valid config is loaded. |

## Response headers

| Header | Meaning |
| --- | --- |
| `X-Queue-Wait-Ms` | Time spent waiting for a slot. |
| `X-Pool-Name` | Pool that served the request. |

Send `X-Priority: <int>` to override the route priority for a single request.

## Errors

Errors use the OpenAI format (`{"error": {"message", "type", "param", "code"}}`):

| Status | Code | Cause |
| --- | --- | --- |
| 400 | `missing_model`, `invalid_json`, `invalid_priority` | Malformed request. |
| 404 | `model_not_found` | The model is not a configured route. |
| 503 | `queue_timeout` | No slot became free within `server.queue_timeout`. |
| 502 | `upstream_error` | The upstream could not be reached. |
