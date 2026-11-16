# Per-protocol endpoints and the checker

A route name can be served on both the OpenAI API and the Anthropic API, each with its own pools and target model. There is no cross-protocol translation: a request is forwarded as-is to a pool speaking the same protocol.

## Manifest

```yaml
pools:
  claude-subscription: {upstream_url: http://bridge:8317, protocol: anthropic, api_key: "${CLAUDE_KEY}", max_concurrency: 2}
  codex-subscription:  {upstream_url: http://bridge:8318, protocol: openai,    api_key: "${CODEX_KEY}",  max_concurrency: 2}
routes:
  fast:
    endpoints:
      anthropic: {pools: [claude-subscription], target_model: "claude-haiku-5-5@low", priority: 10}
      openai:    {pools: [codex-subscription],  target_model: "gpt-6-luna@low",       priority: 10}
  legacy: {pool: codex-subscription, target_model: gpt-5.4}   # shorthand = the openai endpoint
```

- `pools[].protocol` is `anthropic` or `openai` (default `openai`).
- `endpoints` is keyed by protocol; each endpoint takes `pool` or `pools`, `target_model` and `priority`.
- `pool`/`pools`/`target_model`/`priority` directly on the route remain a shorthand for the `openai` endpoint. A route uses either the shorthand or `endpoints`, never both.
- Validation requires every endpoint pool to exist, the list to be non-empty without duplicates, and each pool's `protocol` to equal the endpoint key. Unknown keys are rejected.
- Multi-pool selection, queuing and failover apply within one endpoint's pools.

## Request routing

The client picks the route with the `model` field, as before. The API used picks the endpoint:

| Request path | Endpoint |
| --- | --- |
| `/v1/messages`, `/v1/messages/count_tokens` | `anthropic` |
| everything else under `/v1/` (`/v1/chat/completions`, `/v1/responses`, ...) | `openai` |

If the route has no endpoint for the protocol the proxy answers `404` with code `endpoint_not_configured`, in the error shape of that API (Anthropic: `{"type":"error","error":{"type":"not_found_error",...}}`; OpenAI: `{"error":{...}}`). Streaming works on both.

## Authentication headers

The client's `Authorization` and `x-api-key` are never forwarded. The pool `api_key` is injected as:

- `openai` pools: `Authorization: Bearer <api_key>`
- `anthropic` pools: `x-api-key: <api_key>`

All other headers, notably `anthropic-version` and `anthropic-beta`, are forwarded unchanged. With an empty `api_key` nothing is injected.

## Model listing

`GET /v1/models` lists the routes having an `openai` endpoint in the OpenAI shape. When the request carries an `anthropic-version` header (or `x-api-key` without `Authorization`) it lists the routes having an `anthropic` endpoint in the Anthropic shape (`data`, `has_more`, `first_id`, `last_id`; no pagination parameters are honoured).

## The checker

The image ships a second binary, `/checker`, that validates a manifest with the exact code the proxy loads it with:

```sh
docker run --rm -v "$PWD/config.yaml:/config.yaml:ro" --entrypoint /checker \
  ghcr.io/lerenn/llm-proxy-manifestd:<tag> -f /config.yaml
```

It prints `config ok: N pools, M routes` and exits 0, or prints the error to stderr and exits 1 (2 on usage errors). Use it as an init container, a Helm hook or a CI step. `${VAR}` references are expanded like the proxy does (`os.ExpandEnv`): unset variables become empty and do **not** fail validation, so secrets need not be present. Errors never echo secret values (an invalid `upstream_url` is reported without its value).

## Dashboard

Each route shows its endpoints per protocol (pools, upstream hosts, priority) and each pool its protocol. Only names and host names are exposed.
