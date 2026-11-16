# Providers (subscription sidecars and API keys)

The proxy speaks OpenAI-compatible HTTP to its pools and knows nothing about vendor CLIs. To use a Claude subscription login, run a **CLI adapter** as a sidecar container in the same pod: one adapter per provider. Each adapter is its own pool.

```
pod
├─ llm-proxy-manifestd         :8080  (pools: claude-subscription -> http://localhost:9100)
└─ claude-subscription         127.0.0.1:9100  (adapter + official `claude` CLI, login on a PVC)
```

The Claude adapter speaks the **Anthropic Messages API** (`POST /v1/messages`, streaming SSE and non-stream, plus `GET /ready`), so Claude Code style clients can be served. Claude clients call `/v1/messages` on the proxy, which uses an endpoint under `endpoints.anthropic` (see [Protocol endpoints](./protocol-endpoints)): **a name must be declared under `endpoints.anthropic` to be reachable by Claude clients**, otherwise it answers 404 `endpoint_not_configured`. The adapter listens on loopback only, so it needs no `api_key` and no NetworkPolicy.

::: warning Status
The Claude adapter has only been tested against a fake CLI that emits the documented `stream-json` events. It has **never run against a real subscription login**. Details in [Unverified](#unverified).
:::

::: warning Terms of use
This is meant for a single user running their own subscription. Vendor terms for subscription logins and automated or programmatic use may restrict this; you are responsible for reading and complying with them. Nothing here claims it is permitted.
:::

## Provider types

`providers` is a map; each entry has a `type`:

| type | renders | pool `protocol` | notes |
| :- | :- | :- | :- |
| `claude` | sidecar container + PVC + pool | `anthropic` | Claude subscription login via the `claude` CLI (this page) |
| `anthropic-api` | pool only | `anthropic` | Anthropic API key from a Secret |
| `openai-api` | pool only | `openai` | OpenAI or any OpenAI-compatible gateway, key from a Secret |
| `codex` | not implemented | `openai` | planned |

Unknown types fail the render. The old `accounts` key (and `accountsBasePort`, `accountStorage`) fails the render with a pointer to `providers`.

### API-key providers

```yaml
providers:
  my-anthropic:
    type: anthropic-api
    url: https://api.anthropic.com   # default for this type
    maxConcurrency: 4
    apiKey: {existingSecret: {name: provider-keys, key: anthropic}}   # required
  my-openai:
    type: openai-api
    url: https://api.openai.com      # default for this type
    apiKey: {existingSecret: {name: provider-keys, key: openai}}
```

```sh
kubectl create secret generic provider-keys \
  --from-literal=anthropic="$ANTHROPIC_API_KEY" --from-literal=openai="$OPENAI_API_KEY"
```

The key is never put in values: the chart injects it into the proxy container as `PROVIDER_<NAME>_API_KEY` (name upper-cased, `-` becomes `_`) with `secretKeyRef` (`optional: false`, so a missing Secret blocks the pod) and renders `api_key: ${PROVIDER_<NAME>_API_KEY}` in the pool.

**What the URL should be.** The proxy appends the client's request path (which already starts with `/v1/...`) to the path of `upstream_url`. So use the bare origin: `https://api.anthropic.com`, `https://api.openai.com`. Do **not** add `/v1` (it would become `/v1/v1/...`). For a gateway that lives under a path prefix, give that prefix only, e.g. `https://gw.example.com/openai`. Anthropic pools get `x-api-key`, OpenAI pools `Authorization: Bearer`.

A pod with only API-key providers keeps the normal rolling update and `replicaCount`; the Recreate/replicas 1/hardening/PVC behaviour below applies as soon as one `claude` provider exists. The `check-config` init container runs whenever any provider exists.

### Subscription with an API fallback

List both pools as targets of one Anthropic endpoint. With `mode: failover` the subscription is always used first (requests queue on it when it is busy) and the API pool only takes a request when the subscription fails with a 5xx or transport error (see [Multi-pool routing](./multi-pool-routing)):

```yaml
providers:
  claude-subscription: {type: claude, maxConcurrency: 4}
  my-anthropic: {type: anthropic-api, apiKey: {existingSecret: {name: provider-keys, key: anthropic}}}
config:
  endpoints:
    anthropic:
      standard-medium:
        mode: failover
        priority: 50
        targets:
          - {pool: claude-subscription, target_model: "claude-sonnet-5-5@medium"}
          - {pool: my-anthropic, target_model: "claude-sonnet-5-5"}
```

Use the default `mode: least-inflight` instead to spread load over both pools (the API pool is then used whenever it has fewer requests in flight).

## Helm configuration (claude sidecar)

```yaml
providers:
  claude-subscription:
    type: claude          # only "claude" is supported
    maxConcurrency: 4     # becomes the pool's max_concurrency (default 4)
    # port: 9100          # default: providersBasePort (9100) + index among `claude` providers in name order
    # storage: {size: 1Gi, storageClass: ""}   # overrides providerStorage
    # resources: {}
    # env: []

providerStorage: {size: 1Gi, storageClass: ""}   # defaults for every provider
adapters:
  claude:
    image: {repository: ghcr.io/lerenn/llm-proxy-manifestd-adapter-claude, tag: ""}  # tag defaults to appVersion

config:
  endpoints:
    anthropic:
      fast:            {priority: 10, targets: [{pool: claude-subscription, target_model: "claude-haiku-5-5@low"}]}
      standard-low:    {priority: 50, targets: [{pool: claude-subscription, target_model: "claude-sonnet-5-5@low"}]}
      standard-medium: {priority: 50, targets: [{pool: claude-subscription, target_model: "claude-sonnet-5-5@medium"}]}
      standard-high:   {priority: 50, targets: [{pool: claude-subscription, target_model: "claude-sonnet-5-5@high"}]}
      premium-high:    {priority: 80, targets: [{pool: claude-subscription, target_model: "claude-opus-5-5@high"}]}
      frontier-high:   {priority: 90, targets: [{pool: claude-subscription, target_model: "claude-fable-5-2@high"}]}
```

For every entry in `providers` the chart renders:

- one sidecar container named after the provider, listening on `127.0.0.1:<port>`; ports are assigned deterministically (`providersBasePort` + index among `claude` providers in alphabetical order) unless `port` is set;
- one PVC `<release>-<provider>` mounted at `/data/claude` (`CLAUDE_CONFIG_DIR`), holding the login;
- a pool with the provider's name (`upstream_url: http://localhost:<port>`, `max_concurrency`, and `protocol: anthropic` for claude providers when ``) merged into `config.pools`. A clash with a user-defined pool of the same name fails the render.

The chart's example pool and endpoints (`defaultPools`/`defaultEndpoints`) are only applied when `providers` is empty, so they never interfere with provider setups.

Spread an endpoint over several providers by listing several `targets` (see [Multi-pool routing](./multi-pool-routing)). `priority` and `max_concurrency`/`maxConcurrency` may be omitted (priority `0`, concurrency `4` for providers).

When at least one provider exists the Deployment uses `replicas: 1` and `strategy: Recreate`, `automountServiceAccountToken: false`, non-root users (65532 for the proxy, 10001 for adapters), `readOnlyRootFilesystem`, all capabilities dropped, a `check-config` init container that runs `/checker -f` on the rendered config (from the proxy image), and a per-container readiness probe. With `providers: {}` the chart renders exactly as before.

## Login

The adapter is not ready (`GET /ready` returns 503, so the pod is not Ready and requests get 503) until credentials exist. Log in once per provider:

```sh
kubectl exec -it deploy/<release>-llm-proxy-manifestd -c claude-subscription -- claude
# then type /login and follow the prompts
```

The login is stored on the PVC and survives restarts. Readiness is decided by the presence of `.credentials.json` in the login directory.

## Models and reasoning effort

The model string may carry an `@low`, `@medium` or `@high` suffix, e.g. `claude-sonnet-5-5@medium`. The adapter strips it and passes it to the CLI as `--effort`. Any other suffix is a 400.

Each request runs a fresh `claude` process (`-p`, `--input-format/--output-format stream-json`, `--include-partial-messages`, `--tools ""`, `--no-session-persistence`, no MCP servers, settings sources ignored). Multi-turn chat histories are flattened into one prompt; the `system` field becomes the system prompt; only text blocks are supported. Early failures (before any output) return 5xx so the proxy can fail over to another pool.

## Operations

- **Rollout means a brief outage.** `Recreate` stops the old pod before starting the new one (a login volume is ReadWriteOnce and a provider must not run twice).
- **Adding or removing a provider restarts the pod** (it changes the pod spec). Edits to `config` alone (endpoints, priorities) hot-reload without a restart.
- Uninstalling the release deletes the PVCs and therefore the logins.

## Security notes

- Adapters bind to loopback; only containers in the pod can reach them. No credentials appear in the proxy config.
- The login PVC holds long-lived credentials: restrict access to the namespace, `pods/exec`, and the PVC, and consider encrypted storage.
- Anyone who can reach the proxy can spend the subscription; protect the proxy endpoint accordingly.
- Tools are disabled and no session is persisted, but prompts are sent to the vendor through the CLI.

## Unverified

- The adapter has never run against a real login or the real CLI at all; it is tested with a fake CLI script only.
- The stream-json event shapes (`stream_event` text deltas, `assistant`, `result` with `usage`) are taken from the Claude Code headless documentation, not observed live.
- Using `.credentials.json` as the "logged in" signal, `--effort` behaviour for every model (including Haiku), and the exact error text used to detect an expired login are assumptions.
- Token usage reported by the adapter is whatever the CLI reports and may not match OpenAI semantics.
- Image builds pin Claude Code 2.1.325 (sha256-verified); the image contains no Node.js because Claude Code ships as a native binary.

## Codex (not implemented)

There is no Codex adapter. A design sketch only: a sidecar would spawn `codex app-server` (JSON-RPC over stdio, login dir from `CODEX_HOME`) and expose the same contract. The app-server protocol has **not** been verified, so nothing is implemented.

## Also unverified (API-key providers)

- The `anthropic-api` and `openai-api` pools were validated with the repository's `/checker` only; they have not been exercised against the real APIs or a real cluster Secret.
