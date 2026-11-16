<p align="center"><img src="logo.svg" alt="LLM Proxy ManifestD logo" width="96"></p>

# LLM Proxy ManifestD

A Kubernetes-native LLM reverse proxy, deployed with a Helm chart and configured through a ConfigMap that hot-reloads. It adds priority queuing, per-pool concurrency limits and SSE streaming in front of OpenAI-compatible endpoints, with a read-only status dashboard embedded in the binary and no database. Optional CLI sidecars (Claude, Codex; experimental) can run alongside it in the same pod.

> **Kubernetes-first.** The supported way to run this project is the Helm chart: the manifest is a ConfigMap that hot-reloads, and the proxy runs as a regular pod next to your upstream bridges. Optional CLI sidecars for Claude and Codex subscription logins run in the same pod and are enabled through the chart's `providers:` value (see [Subscription providers](https://llm-proxy-manifestd.lerenn.dev/guide/subscription-providers)); both adapters are experimental and have never run against a real login. Sidecar-less API providers (`anthropic-api`, `openai-api`) also exist. The binary itself is a static Go program that reads a config file, so it can also run under Docker or on a laptop, but that is not the primary target.

Everything is declared in a single YAML manifest delivered through Helm. There is nothing to click, nothing to migrate, and no state to back up: change your values, run `helm upgrade`, and the running proxy reloads the new manifest without dropping a single in-flight stream.

## Install

Create a secret with the credentials referenced as `${VAR}` in the manifest:

```sh
kubectl create secret generic llm-proxy-keys \
  --from-literal=LOCAL_BRIDGE_KEY=change-me
```

Describe your pools and endpoints in `values.yaml`:

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

Install, then upgrade whenever the manifest changes:

```sh
helm install llm-proxy oci://ghcr.io/lerenn/charts/llm-proxy-manifestd -f values.yaml
helm upgrade llm-proxy oci://ghcr.io/lerenn/charts/llm-proxy-manifestd -f values.yaml
```

Clients then talk OpenAI-compatible HTTP to the service and use an endpoint name (e.g. `codex-interactive`) as the model. `GET /v1/models` lists the endpoint names.

## Concurrency limit (default: unlimited)

`max_concurrency` limits parallel requests per pool. **Omitting it, or setting `0`, means unlimited** (no queue, requests go straight through); this is a change from earlier releases, where it was required and had to be >= 1. Negative values are invalid. The same applies to `maxConcurrency` on chart `providers`. Set a positive number wherever you rely on a limit.

## Endpoints, protocols and multi-pool routing

Endpoints are declared per protocol: `endpoints.anthropic` serves `/v1/messages` (and `/v1/messages/count_tokens`), `endpoints.openai` every other `/v1/` path. The endpoint name is the `model` clients send; a name missing for the request's protocol returns 404 `endpoint_not_configured` in that API's error shape. There is no protocol translation, and every target pool must declare the endpoint's `protocol` (`anthropic` or `openai`, default `openai`).

```yaml
endpoints:
  anthropic:
    standard-medium:
      mode: least-inflight   # default; or round-robin, failover
      priority: 50           # optional, default 0
      targets:               # several accounts, or fallbacks
        - {pool: claude-a, target_model: "claude-sonnet-5-5@medium"}
        - {pool: claude-b, target_model: "claude-sonnet-5-5@medium", priority: 40}   # per-target priority, optional
  openai:
    codex-default: {targets: [{pool: codex-sub, target_model: gpt-5.4}]}
```

`least-inflight` takes the target with the fewest in-flight requests that has a free slot, `round-robin` rotates the starting target, `failover` sticks to the first target and only moves on when it fails. In every mode a transport error or 5xx before any response byte is retried on the next untried target; 4xx is never retried. `target_model` is opaque (an `@medium` suffix means nothing to the proxy). The pool key is sent as `Authorization: Bearer` (openai) or `x-api-key` (anthropic); the client's credentials are never forwarded.

`GET /v1/models` lists the endpoint names of the caller's protocol: Anthropic shape when the request has an `anthropic-version` or `x-api-key` header, OpenAI shape otherwise.

The top-level `routes:` key of earlier versions was removed; a manifest still using it fails with a pointer to the new form. Details: [Protocol endpoints](https://llm-proxy-manifestd.lerenn.dev/guide/protocol-endpoints), [Multi-pool routing](https://llm-proxy-manifestd.lerenn.dev/guide/multi-pool-routing), [Using opencode](https://llm-proxy-manifestd.lerenn.dev/guide/opencode).

## Checker

The image also contains `/checker`, which validates a manifest with the proxy's own loader: `docker run --rm -v $PWD/config.yaml:/config.yaml --entrypoint /checker <image> -f /config.yaml`. Exit 0 if valid, 1 with a readable error otherwise. Unset `${VAR}`s expand to empty and do not fail validation; secrets are never printed. See [Protocol endpoints](https://llm-proxy-manifestd.lerenn.dev/guide/protocol-endpoints).

## Subscription providers (CLI adapters)

To use a Claude subscription login, define `providers:` in the Helm values. Each provider becomes a loopback-only adapter sidecar (Anthropic Messages API, one per provider, its own pool), a PVC for the login and a matching pool, and the pod switches to `replicas: 1` / `Recreate`.

```yaml
providers:
  claude-subscription: {type: claude, maxConcurrency: 4}
```

Log in with `kubectl exec -it deploy/<release>-llm-proxy-manifestd -c claude-subscription -- claude` then `/login`. Rollouts cause a brief outage; adding a provider restarts the pod, config edits hot-reload. The adapter has not been run against a real subscription yet, and using a subscription this way may be restricted by the vendor's terms: it is for a single user with their own account, and you must check those terms yourself. See the [guide](website/docs/guide/subscription-providers.md).

## Client API keys

Define `clients:` in the manifest to require an API key (`x-api-key` or `Authorization: Bearer`) on `/v1/` calls; each client has an optional default `priority` and endpoint-name allow-list. The chart generates one key per client into a Secret (kept across upgrades via `lookup`; use `clientsExistingSecret` with `helm template`/Argo CD). Keys never appear in the ConfigMap. See [Client API keys](website/docs/guide/client-keys.md).

## lpmctl (CLI)

`lpmctl` runs on your machine with your kubeconfig (it is not in any image): `lpmctl keys [client]` prints the client keys (only the key with a name, pipe-friendly), `lpmctl status` shows each provider's login state and the configured clients, `lpmctl login <provider>` opens the interactive CLI login in a subscription sidecar. Binaries for linux/darwin (amd64, arm64) with sha256 checksums are attached to each [GitHub release](https://github.com/lerenn/llm-proxy-manifestd/releases). See the [lpmctl guide](https://llm-proxy-manifestd.lerenn.dev/guide/lpmctl) (flags, RBAC).

## Dashboard

The binary embeds a lightweight, read-only dashboard (status, version, uptime, slots, queue depth per priority, per-endpoint in-flight/queued/requests/errors, last config reload). It is public and shows only names, upstream host names, counts and timings, never keys, URLs, prompts or headers.

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
