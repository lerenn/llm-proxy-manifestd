# Using opencode

[opencode](https://opencode.ai) can talk to the proxy as a custom provider. The model names are the proxy's **route names** (`fast`, `standard-medium`, ...), and a route needs an endpoint for the protocol the provider uses (see [Protocol endpoints](./protocol-endpoints)).

## Does opencode discover models from `/v1/models`?

Short answer: **not for a generic custom provider.** From a read of the opencode sources (repository HEAD at the time of writing, `services/site/src/docs/content/{providers,models}.mdx` and `packages/core/src/plugin/provider/`):

- A custom provider (`package: "@opencode/ai/providers/openai-compatible"` or `.../anthropic-compatible`, or the V1 `npm: "@ai-sdk/openai-compatible"`) needs its models **listed explicitly** in the config: the docs say to define "its provider package, endpoint, and at least one model", and nothing calls `/v1/models` for such a provider.
- Automatic discovery exists only for specific built-in local providers: `ollama` (native Ollama API), `lmstudio`, and `vllm`. The vLLM plugin does call `GET /v1/models`, but it only keeps model cards whose `owned_by` is `vllm`, and it first checks `GET /health`. The proxy reports `owned_by: "llm-proxy-manifestd"` and has no `/health` route, so it is not picked up as `vllm`, and masquerading as one is not recommended.

So the proxy still serves `GET /v1/models` (route names, OpenAI or Anthropic shape), which is useful for other clients and for copying the list, but with opencode you list the models by hand. This repository does not ship a generator for that.

## OpenAI-compatible routes

Current opencode config format (`providers`, `package`, `settings`):

```jsonc title="opencode.jsonc"
{
  "$schema": "https://opencode.ai/config.json",
  "model": "proxy/fast",
  "providers": {
    "proxy": {
      "name": "llm-proxy-manifestd",
      "package": "@opencode/ai/providers/openai-compatible",
      "settings": {
        "baseURL": "https://llm-proxy.example.com/v1"
        // "apiKey": "{env:PROXY_KEY}"   // only if you put your own auth in front of the proxy
      },
      "models": {
        "fast": {},
        "standard-medium": {}
      }
    }
  }
}
```

`baseURL` must end in `/v1`: opencode appends `/chat/completions`. The proxy ignores the client's `Authorization` and uses the pool's key, so no key is needed unless you add authentication at your ingress.

## Anthropic (Claude) routes

Routes with an `anthropic` endpoint (for example a [Claude subscription provider](./subscription-providers)) use the Anthropic-compatible package. The runtime appends `/messages` to `baseURL` (so keep the `/v1`) and sends the key as `x-api-key`:

```jsonc title="opencode.jsonc"
{
  "$schema": "https://opencode.ai/config.json",
  "model": "proxy-claude/standard-medium",
  "providers": {
    "proxy-claude": {
      "name": "llm-proxy-manifestd (Anthropic)",
      "package": "@opencode/ai/providers/anthropic-compatible",
      "settings": { "baseURL": "https://llm-proxy.example.com/v1" },
      "models": {
        "standard-medium": {},
        "premium-high": {}
      }
    }
  }
}
```

## Older (V1) config format

Older opencode versions use `provider`, `npm` and `options.baseURL` instead; opencode's own migration notes map `npm` to `package` (AI SDK packages get an `aisdk:` prefix), `options.baseURL`/`api` to `settings.baseURL`. For an OpenAI-compatible route:

```json
{
  "provider": {
    "proxy": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "llm-proxy-manifestd",
      "options": { "baseURL": "https://llm-proxy.example.com/v1" },
      "models": { "fast": {}, "standard-medium": {} }
    }
  }
}
```

## Reasoning effort and model suffixes

The `@low|@medium|@high` suffix belongs in the route's `target_model` on the proxy side, not in the opencode model ID: define one route per effort (`standard-low`, `standard-medium`, ...) and list each as an opencode model.

## What is verified

- Verified from the opencode sources: discovery scope (above), the `package` names, that `settings.baseURL` is combined with `/chat/completions` (OpenAI-compatible) or `/messages` (Anthropic-compatible), and the V1 to V2 key mapping from its migration guide.
- Not verified: I did not run opencode against the proxy. The model entries above are minimal (`{}`); depending on your opencode version you may want `limit` and `capabilities` per model (see opencode's [models guide](https://opencode.ai/docs/models)). The opencode config schema changes between releases; check its current docs.
