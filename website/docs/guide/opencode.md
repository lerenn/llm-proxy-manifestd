# Using opencode

[opencode](https://opencode.ai) can talk to the proxy as a custom provider. Its model IDs are the proxy's **endpoint names** (`fast`, `standard-medium`, ...) for the protocol the provider package speaks: an Anthropic-compatible provider sees `endpoints.anthropic`, an OpenAI-compatible one sees `endpoints.openai` (see [Protocol endpoints](./protocol-endpoints)).

## Does opencode discover models from `/v1/models`?

**No, not for a custom provider.** You must list the models in the opencode config by hand. From a read of the opencode sources and docs (`services/site/src/docs/content/{providers,models}.mdx`, `packages/core/src/plugin/provider/`):

- A custom provider needs its models **listed explicitly**: the docs ask for "its provider package, endpoint, and at least one model", and nothing calls `/v1/models` for such a provider.
- Automatic discovery exists only for specific built-in local providers: `ollama` (native Ollama API), `lmstudio` and `vllm`. The vLLM plugin does call `GET /v1/models`, but it first checks `GET /health` and only keeps entries whose `owned_by` is `vllm`. The proxy reports `owned_by: "llm-proxy-manifestd"` and has no `/health` route, so it is not picked up that way, and pretending to be vLLM is not recommended.

The proxy's `GET /v1/models` is still the source of truth for what to list. Ask it with the headers of the protocol you will use:

```sh
# Anthropic endpoint names
curl -s -H 'anthropic-version: 2023-06-01' https://llm-proxy.example.com/v1/models
# OpenAI endpoint names
curl -s https://llm-proxy.example.com/v1/models
```

Rather than copying the ids by hand, generate the `models` maps with [`lpmctl opencode`](./lpmctl#lpmctl-opencode):

```sh
export LPMCTL_PROXY_URL=https://llm-proxy.example.com/v1
lpmctl opencode --client opencode            # dry run: shows what would change
lpmctl opencode --client opencode --write    # apply, then: opencode service restart
```

Re-run it whenever the endpoints of the proxy change. Without a `models` map opencode fails with `Model unavailable: <provider>/<model>`.

## Claude subscription (Anthropic endpoints)

With a [Claude subscription provider](./subscription-providers) and these Helm values:

```yaml
providers:
  claude-subscription: {type: claude, storage: {size: 1Gi}}
config:
  endpoints:
    anthropic:
      fast:            {targets: [{pool: claude-subscription, target_model: "claude-haiku-5-5@low"}]}
      standard-medium: {targets: [{pool: claude-subscription, target_model: "claude-sonnet-5-5@medium"}]}
      premium-high:    {targets: [{pool: claude-subscription, target_model: "claude-opus-5-5@high"}]}
```

use an Anthropic-compatible provider. The runtime appends `/messages` to `baseURL`, so keep the `/v1`:

```jsonc title="opencode.jsonc"
{
  "$schema": "https://opencode.ai/config.json",
  "model": "proxy-claude/standard-medium",
  "providers": {
    "proxy-claude": {
      "name": "llm-proxy-manifestd (Anthropic)",
      "package": "@opencode/ai/providers/anthropic-compatible",
      "settings": {
        "baseURL": "https://llm-proxy.example.com/v1",
        "apiKey": "unused"
      },
      "models": {
        "fast": {},
        "standard-medium": {},
        "premium-high": {}
      }
    }
  }
}
```

The proxy never forwards the client's `x-api-key` or `Authorization` (it injects the pool's own credentials, or none for a subscription sidecar), so `apiKey` only needs to be non-empty for SDKs that insist on one. Use a real secret (`"{env:PROXY_KEY}"`) only if you put authentication in front of the proxy.

## OpenAI-compatible endpoints

```jsonc title="opencode.jsonc"
{
  "$schema": "https://opencode.ai/config.json",
  "model": "proxy/codex-default",
  "providers": {
    "proxy": {
      "name": "llm-proxy-manifestd",
      "package": "@opencode/ai/providers/openai-compatible",
      "settings": { "baseURL": "https://llm-proxy.example.com/v1" },
      "models": { "codex-default": {}, "codex-interactive": {} }
    }
  }
}
```

`baseURL` must end in `/v1`: opencode appends `/chat/completions`.

## Older (V1) config format

Older opencode versions use `provider`, `npm` and `options` instead; opencode's own migration notes map `npm` to `package` (AI SDK packages get an `aisdk:` prefix) and `options.baseURL` to `settings.baseURL`. The same two providers in that format:

```json
{
  "provider": {
    "proxy-claude": {
      "npm": "@ai-sdk/anthropic",
      "name": "llm-proxy-manifestd (Anthropic)",
      "options": { "baseURL": "https://llm-proxy.example.com/v1", "apiKey": "unused" },
      "models": { "fast": {}, "standard-medium": {}, "premium-high": {} }
    },
    "proxy": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "llm-proxy-manifestd",
      "options": { "baseURL": "https://llm-proxy.example.com/v1" },
      "models": { "codex-default": {}, "codex-interactive": {} }
    }
  }
}
```

## Reasoning effort and model suffixes

The `@low|@medium|@high` suffix belongs in a target's `target_model` on the proxy side, not in the opencode model ID: define one endpoint per effort (`standard-low`, `standard-medium`, ...) and list each as an opencode model.

## What is verified

- Verified from the opencode sources: the discovery scope above, the `package` names, that `baseURL` is combined with `/chat/completions` (OpenAI-compatible) or `/messages` (Anthropic-compatible), and the V1 to V2 key mapping from its migration guide.
- Verified in this repository's tests: the `GET /v1/models` shapes and the header-based protocol choice.
- Verified: opencode v2.3.0 lists (`opencode models`) the models generated by `lpmctl opencode` in a V1-style (`provider`/`npm`/`options`) config, with minimal `{"name": ...}` entries.
- Not verified: opencode was not run against a real proxy for the hand-written V2 examples. The model entries above are minimal (`{}`); depending on your opencode version you may want `limit` and `capabilities` per model (see opencode's [models guide](https://opencode.ai/docs/models)). The opencode config schema changes between releases; check its current docs.
