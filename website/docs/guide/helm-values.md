# Helm Values

Chart: `oci://ghcr.io/lerenn/charts/llm-proxy-manifestd`.

| Value | Default | Description |
| --- | --- | --- |
| `image.repository` | `ghcr.io/lerenn/llm-proxy-manifestd` | Image repository. |
| `image.tag` | `""` (chart `appVersion`) | Image tag. |
| `image.pullPolicy` | `IfNotPresent` | Pull policy. |
| `existingSecret` | `""` | Secret exposed with `envFrom`, so `${VAR}` in the manifest resolve. |
| `replicaCount` | `1` | Replicas. See the note below. |
| `terminationGracePeriodSeconds` | `600` | Time given to in-flight streams on shutdown. |
| `resources` | `{}` | Container resources. |
| `service.type` / `service.port` | `ClusterIP` / `80` | Service settings. |
| `ingress.enabled` | `false` | Create an Ingress. |
| `ingress.className`, `.annotations`, `.hosts`, `.tls` | | Standard ingress settings. |
| `dashboard.ingress.enabled` | `false` | Create a second Ingress for the [dashboard](./dashboard). |
| `dashboard.ingress.host` | `""` | Dashboard hostname (should start with `dashboard.`, or match `config.dashboard.host`). |
| `dashboard.ingress.className`, `.annotations`, `.tls` | falls back to `ingress.*` | Settings for the dashboard Ingress. |
| `config` | example manifest | Rendered with `toYaml` into `config.yaml`. |

::: warning Replicas
Concurrency limits are enforced **per process**. With `replicaCount: 2` and `max_concurrency: 2`,
the upstream account may see up to 4 parallel requests. Keep one replica for strict limits.
:::

::: tip Overriding the default manifest
Helm merges maps, so your `config.pools` are added to the chart's example pools. Remove an
example entry by setting it to `null`, e.g. `config.pools.codex-sub: null`.
:::

## Manifest reference (`config`)

```yaml
server:
  listen: ":8080"          # restart required to change
  queue_timeout: "120s"    # max wait for a slot before a 503
  aging_interval: "5s"     # optional; +1 priority per interval waited (0 / unset: off)
pools:
  <name>:
    upstream_url: http://cliproxyapi.default.svc.cluster.local:8317
    api_key: "${LOCAL_BRIDGE_KEY}"   # sent as "Authorization: Bearer ..."
    max_concurrency: 2               # >= 1
dashboard:
  enabled: true            # default true
  host: ""                  # exact dashboard hostname; default: any host starting with "dashboard."
  path: /dashboard         # prefix on every host; "off" disables
endpoints:
  <anthropic|openai>:      # anthropic: /v1/messages; openai: every other /v1/ path
    <name>:                # the model name clients send
      mode: least-inflight # optional: least-inflight (default), round-robin, failover
      priority: 50         # optional, default 0; higher is served first
      targets:             # one or more, tried per `mode`, failover on 5xx/transport error
        - pool: <pool name>          # must have the endpoint's protocol
          target_model: gpt-5.4      # optional rewrite of "model"
          # priority: 60             # optional per-target override
```

The pre-endpoints top-level `routes:` key (and the chart's `defaultRoutes` value) were removed and
are rejected with a migration hint; see [Protocol endpoints](./protocol-endpoints).

Environment variables are expanded (`os.ExpandEnv`) on the raw YAML before parsing. Unknown keys,
unknown or wrong-protocol pools in endpoint targets, `max_concurrency < 1`, bad URLs and bad durations are all rejected.

## Deployment behaviour

- The ConfigMap is mounted as a directory (no `subPath`) so Kubernetes' atomic symlink swap is
  visible to the process.
- The Deployment has **no** `checksum/config` annotation: a values change hot-reloads instead of
  rolling the pod.
- `RollingUpdate` with `maxUnavailable: 0`, `maxSurge: 1`, a 3 second `preStop` sleep and a 600 s
  grace period let old pods finish their streams while new pods take over.
