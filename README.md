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

## Multi-pool routing

A route can spread requests over several pools (for example several accounts of the same provider). Use `pools:` instead of `pool:`; the two keys are mutually exclusive.

```yaml
pools:
  claude-a: {upstream_url: http://bridge-a:8317, api_key: "${KEY_A}", max_concurrency: 2}
  claude-b: {upstream_url: http://bridge-b:8317, api_key: "${KEY_B}", max_concurrency: 2}
routes:
  standard-medium: {pools: [claude-a, claude-b], target_model: "claude-sonnet-5-5@medium", priority: 50}
  codex-default:   {pool: codex-sub, target_model: gpt-5.4, priority: 50}   # single pool still works
```

Validation rejects unknown pools, an empty `pools: []`, duplicates, a route with both `pool` and `pools`, and a route with neither.

Exact behaviour:

1. **Selection.** Among the route's pools, the proxy takes a slot on the pool with the fewest in-flight requests (held slots) that has a free slot. Ties go to the pool listed first.
2. **All pools full.** The request queues (with its priority) on the pool with the lowest in-flight plus queued count; ties go to the pool listed first. Once queued it stays on that pool until `queue_timeout`.
3. **Failover.** If the chosen pool fails with a transport error (connect failure, reset before response headers) or answers 5xx, the proxy releases the slot and retries the same request on the next pool, chosen with the same rules among the pools not yet tried. 4xx responses are never retried. Failover only happens before any response byte reaches the client: once headers are forwarded (streaming started), a later upstream failure is not retried.
4. **Exhausted.** If every pool failed, the last attempt's result is returned (its 5xx response, or `502 upstream_error` for a transport error).
5. `X-Pool-Name` reports the pool that finally served the request.

Config changes, including a route's `pools`, are applied on hot-reload; in-flight requests finish on the pools they started with.

`target_model` is an opaque string passed upstream unchanged. A suffix such as `@low`, `@medium` or `@high` (e.g. `claude-sonnet-5-5@medium`) has no meaning to the proxy; interpreting it is up to the upstream.

The dashboard shows each route's `pools` and their upstream host names (the JSON also keeps `pool`/`upstream` for the first pool).

## Per-protocol endpoints

Pools declare `protocol: anthropic|openai` (default `openai`). A route may serve both APIs with `endpoints:` keyed by protocol; `pool`/`pools`/`target_model`/`priority` on the route stay a shorthand for the `openai` endpoint (use one form or the other).

```yaml
routes:
  fast:
    endpoints:
      anthropic: {pools: [claude-subscription], target_model: "claude-haiku-5-5@low", priority: 10}
      openai:    {pools: [codex-subscription],  target_model: "gpt-6-luna@low",       priority: 10}
```

`/v1/messages` and `/v1/messages/count_tokens` use the `anthropic` endpoint, other `/v1/` calls the `openai` one; a missing endpoint returns 404 `endpoint_not_configured` in the API's error shape. There is no protocol translation. The pool key is sent as `Authorization: Bearer` (openai) or `x-api-key` (anthropic); `anthropic-version`/`anthropic-beta` pass through.

## Checker

The image also contains `/checker`, which validates a manifest with the proxy's own loader: `docker run --rm -v $PWD/config.yaml:/config.yaml --entrypoint /checker <image> -f /config.yaml`. Exit 0 if valid, 1 with a readable error otherwise. Unset `${VAR}`s expand to empty and do not fail validation; secrets are never printed. See [Protocol endpoints](https://llm-proxy-manifestd.lerenn.dev/guide/protocol-endpoints).

## Subscription accounts (CLI adapters)

To use a Claude subscription login, define `accounts:` in the Helm values. Each account becomes a loopback-only adapter sidecar (Anthropic Messages API, one per account, its own pool), a PVC for the login and a matching pool, and the pod switches to `replicas: 1` / `Recreate`.

```yaml
accounts:
  claude-subscription: {type: claude, maxConcurrency: 4}
```

Log in with `kubectl exec -it deploy/<release>-llm-proxy-manifestd -c claude-subscription -- claude` then `/login`. Rollouts cause a brief outage; adding an account restarts the pod, config edits hot-reload. The adapter has not been run against a real subscription yet, and using a subscription this way may be restricted by the vendor's terms: it is for a single user with their own account, and you must check those terms yourself. See the [guide](website/docs/guide/subscription-accounts.md).

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
