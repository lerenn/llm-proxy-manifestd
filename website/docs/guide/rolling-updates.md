# Rolling updates

By default, a proxy with a subscription provider (`claude` / `codex`) restarts with a brief outage: the CLI adapters run as sidecars in the proxy pod, their login lives on a ReadWriteOnce volume, so the Deployment is `replicas: 1` with `strategy: Recreate`. Two opt-in chart values remove that limit: `adapters.mode: separate` and `valkey.enabled`.

## What each mode does

| Topology | Proxy Deployment | Notes |
| --- | --- | --- |
| No `claude`/`codex` provider (API providers only) | `RollingUpdate` (surge 1, unavailable 0), `replicaCount` replicas | Unchanged. Limits are per process unless you add a shared limiter. |
| `adapters.mode: sidecar` (default) + `claude`/`codex` | `replicas: 1`, `Recreate` | Unchanged: sidecars on `127.0.0.1`, login PVCs mounted in the proxy pod, no token. Rollout = brief outage. |
| `adapters.mode: separate`, **no** shared limiter | `replicas: 1`, `Recreate` | A surge pod would double the per-pod concurrency limits. |
| `adapters.mode: separate` **and** (`valkey.enabled` **or** `config.coordination` set) | `RollingUpdate` (surge 1, unavailable 0), `replicaCount` replicas | The new pod is Ready before the old one drains. |

`replicaCount > 1` with a subscription provider is allowed only in the last row; otherwise the chart fails to render. The proxy Service only routes to Ready proxy pods; `terminationGracePeriodSeconds` and the preStop hook are unchanged, so in-flight streams finish.

## `adapters.mode: separate`

Each subscription provider gets its own Deployment (1 replica, `Recreate`, its login PVC, same hardening as the sidecar) and a ClusterIP Service `<fullname>-<provider>`. The pool's `upstream_url` becomes `http://<fullname>-<provider>:<port>`. The proxy pod has no PVC and no adapter container. Adapters never gate readiness: an adapter that is not logged in does not make the proxy NotReady, so the proxy can roll while a login is pending.

Rolling the **adapter** pod itself still restarts it (Recreate, one login at a time); only the proxy rolls with surge.

### Adapter token

Adapters listen on the pod IP in this mode, so any pod in the cluster could otherwise spend your subscription. They therefore require a shared token: every request except `GET /ready` needs `Authorization: Bearer <token>`, else **401**. (One header for both adapters, including the Anthropic shape of `claude`.)

The token is a Secret `<fullname>-adapter-token` (key `token`) created at first start by the `keys-manager` init container, like the [client keys](/guide/client-keys): created if absent, never overwritten, not owned by Helm or Argo CD (no `lookup`). Its RBAC is limited by `resourceNames` (Kubernetes cannot restrict `create` by name). The proxy receives it as `ADAPTER_TOKEN` and sends it as the pool's `api_key` with `auth: bearer`; adapter pods mount it from the same Secret. Rotate it by deleting the Secret and restarting the proxy and adapter pods.

Adapter pods carry the label `app.kubernetes.io/name: llm-proxy-manifestd-adapter` (and `app.kubernetes.io/component: adapter`) so the proxy Deployment and Service never select them. [lpmctl](/guide/lpmctl) finds them through these labels.

## Valkey

`valkey.enabled: true` renders one Valkey Deployment (1 replica, `Recreate`, non-root, read-only root filesystem, `emptyDir`, pinned image tag) and a ClusterIP Service `<fullname>-valkey`. The chart injects into the proxy config (unless you set `config.coordination` yourself):

```yaml
coordination:
  valkey:
    address: "<fullname>-valkey:6379"
    password_env: VALKEY_PASSWORD   # only with valkey.existingSecret
```

An optional password comes from `valkey.existingSecret` (`name`, `key`, default key `password`): it is passed as the `VALKEY_PASSWORD` environment variable to both Valkey (`--requirepass` from the variable, never in the pod args) and the proxy. Use your own Valkey instead by setting `config.coordination` (other keys: `key_prefix`, `lease_ttl`, `fail_mode: open | closed`).

### Downsides

- **Single point of failure.** One Valkey pod. What happens while it is down depends on `fail_mode`: `open` keeps serving with local-only limits (limits can be exceeded), `closed` rejects requests.
- **A Valkey restart loses leases and queue.** No persistence by default; leases expire (`lease_ttl`) and queued requests are lost.
- **Old and new proxies share Valkey during a rollout**, so the key layout and Lua scripts must stay compatible between two consecutive proxy versions.

## Example

```yaml
adapters:
  mode: separate
valkey:
  enabled: true
replicaCount: 2
providers:
  claude-subscription: {type: claude, maxConcurrency: 4}
  codex-subscription: {type: codex, maxConcurrency: 2}
  nevermetered:
    type: api
    format: [anthropic, openai]
    url: https://api.nevermetered.com
    maxConcurrency: 1
    auth: bearer
    apiKey: {existingSecret: {name: my-secret, key: nevermetered}}
```

The proxy-side behaviour (leases, queue, `fail_mode`, key layout) is documented in [Coordination](/guide/coordination). The first rollout that turns coordination on overlaps old (local) and new (coordinated) proxies: expect one window of double limits.

See also [Subscription providers](/guide/subscription-providers) and [Helm Values](/guide/helm-values).
