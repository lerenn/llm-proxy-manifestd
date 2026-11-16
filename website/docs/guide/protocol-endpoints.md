# Per-protocol endpoints and the checker

Clients pick what to call with the request's `model` field. The proxy looks that name up among the **endpoints of the request's protocol**: an endpoint name can be served on the OpenAI API, the Anthropic API, or both, each with its own targets. There is no cross-protocol translation: a request is forwarded as-is to a pool speaking the same protocol.

## Manifest

```yaml
pools:
  claude-subscription: {upstream_url: http://localhost:9100, protocol: anthropic, max_concurrency: 4}
  codex-subscription:  {upstream_url: http://bridge:8318, protocol: openai, api_key: "${CODEX_KEY}", max_concurrency: 2}
endpoints:
  anthropic:                       # served on /v1/messages
    fast:                          # the `model` clients send
      priority: 10
      targets:
        - {pool: claude-subscription, target_model: "claude-haiku-5-5@low"}
  openai:                          # served on every other /v1/ path
    fast:
      mode: least-inflight         # default; or round-robin, failover
      priority: 10
      targets:
        - {pool: codex-subscription, target_model: "gpt-6-luna@low"}
```

In the Helm chart the same block goes under `config.endpoints`.

| Key | Meaning |
| --- | --- |
| `endpoints.<protocol>` | `anthropic` or `openai`. Any other key is rejected. |
| `endpoints.<protocol>.<name>` | The endpoint name, i.e. the `model` value clients send. |
| `mode` | How a target is picked: `least-inflight` (default), `round-robin` or `failover`. See [Multi-pool routing](./multi-pool-routing). |
| `priority` | Queue priority of the endpoint's requests (higher first). Optional, default `0`. |
| `targets` | Ordered, non-empty list of `{pool, target_model, priority?}`. |
| `targets[].pool` | A pool whose `protocol` equals the endpoint's protocol. |
| `targets[].target_model` | Model sent upstream for this target, an opaque string (`@low`/`@medium`/`@high` suffixes mean nothing to the proxy). Empty keeps the client's `model`. |
| `targets[].priority` | Optional queue priority for requests queued on this target; overrides the endpoint `priority`. |

- `pools[].protocol` is `anthropic` or `openai` (default `openai`); `max_concurrency` is required (>= 1).
- Validation rejects unknown pools, pools of the other protocol, an empty `targets` list, duplicate `{pool, target_model}` pairs, unknown modes and unknown keys (for example `pools:` instead of `targets:`).
- The top-level `routes:` key of earlier versions **is removed**: a manifest still using it fails to load with an error showing the new form. Migration: `routes.<name>.endpoints.<protocol>: {pools: [a, b], target_model: m, priority: p}` becomes `endpoints.<protocol>.<name>: {priority: p, targets: [{pool: a, target_model: m}, {pool: b, target_model: m}]}`; the old route shorthand (`pool`/`pools` directly on the route) was the `openai` endpoint.

## Request resolution

The API used picks the protocol, the `model` field picks the endpoint name:

| Request path | Protocol |
| --- | --- |
| `/v1/messages`, `/v1/messages/count_tokens` | `anthropic` |
| everything else under `/v1/` (`/v1/chat/completions`, `/v1/responses`, ...) | `openai` |

If no endpoint of that name exists for the protocol, the proxy answers `404` with code `endpoint_not_configured`, in the error shape of that API (Anthropic: `{"type":"error","error":{"type":"not_found_error",...}}`; OpenAI: `{"error":{"code":"endpoint_not_configured",...}}`). Streaming works on both.

## Authentication headers

The client's `Authorization` and `x-api-key` are never forwarded. The pool `api_key` is injected as:

- `openai` pools: `Authorization: Bearer <api_key>`
- `anthropic` pools: `x-api-key: <api_key>`

All other headers, notably `anthropic-version` and `anthropic-beta`, are forwarded unchanged. With an empty `api_key` nothing is injected.

## Model listing: `GET /v1/models`

`GET /v1/models` lists the endpoint names configured for the caller's protocol, sorted by name. The protocol is chosen from the headers, since the path is the same for both APIs:

- **Anthropic** when the request carries an `anthropic-version` or an `x-api-key` header (Anthropic SDKs always send one of them):

  ```json
  {"data": [{"type": "model", "id": "fast", "display_name": "fast", "created_at": "1970-01-01T00:00:00Z"}],
   "has_more": false, "first_id": "fast", "last_id": "fast"}
  ```

  `first_id`/`last_id` are `null` when the list is empty. Pagination parameters are ignored: everything is returned in one page.
- **OpenAI** otherwise:

  ```json
  {"object": "list", "data": [{"id": "fast", "object": "model", "created": 0, "owned_by": "llm-proxy-manifestd"}]}
  ```

The list follows hot-reloads. See [Using opencode](./opencode) for what a coding agent does (and does not do) with it.

## The checker

The image ships a second binary, `/checker`, that validates a manifest with the exact code the proxy loads it with:

```sh
docker run --rm -v "$PWD/config.yaml:/config.yaml:ro" --entrypoint /checker \
  ghcr.io/lerenn/llm-proxy-manifestd:<tag> -f /config.yaml
```

It prints `config ok: N pools, A anthropic endpoints, O openai endpoints` and exits 0, or prints the error to stderr and exits 1 (2 on usage errors). The Helm chart runs it as an init container. `${VAR}` references are expanded like the proxy does (`os.ExpandEnv`): unset variables become empty and do **not** fail validation, so secrets need not be present. Errors never echo secret values (an invalid `upstream_url` is reported without its value).

Warnings go to stderr without changing the exit code. When the manifest defines endpoints for **both** protocols, the checker warns about every name present under only one of them (`warning: endpoint "x" exists under anthropic but not openai: ...`), since clients of the other API get a 404 for it. A manifest serving a single protocol produces no such warning.

## Dashboard

The dashboard lists one row per endpoint: name, protocol, mode, targets (pool and target model), their upstream hosts and priorities, plus live counters. Each pool shows its protocol. Only names and host names are exposed.
