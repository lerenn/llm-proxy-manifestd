# Dashboard

A lightweight, read-only web dashboard is embedded in the binary (plain HTML/CSS/JS from `ui/`, no
build step) and served by the proxy itself. It polls every 2 seconds and follows
`prefers-color-scheme`.

A left sidebar switches between four views; the current view is kept in the URL hash
(`#/overview`, `#/pools`, `#/endpoints`, `#/live`) so links and reloads work, and it defaults to Overview. On
narrow screens the sidebar collapses behind a menu button. It works the same on the dashboard host
(`/`) and under the `/dashboard` mount.

- **Overview**: a list of top-level facts: version, uptime, number of pools, endpoints and
  concurrency groups, requests in flight, queued requests, slots in use / total (`unlimited` when a
  pool has no limit), requests / errors, clients (names and request counts, never keys) and the
  config generation with the last reload result (including whether the latest attempt was rejected).
- **Pools**: a table with name, protocol, upstream host, concurrency group, slots used / max,
  queue depth, queue by priority and requests served.
- **Endpoints**: a table per endpoint with protocol, mode, priority, ordered targets
  (pool → target model), upstream hosts, in flight, queued, requests and errors.
- **Live View**: a live node graph, see below.

The overall health badge stays visible in the header. Request and error counters are per process and
reset on restart.

## Live View

`#/live` draws every route (endpoint, grouped by protocol) as a box on the left and every provider
(pool) as a container on the right holding one box per model (distinct `target_model` used in that
pool; the route name when a target has none). One arrow goes from each route to each of its targets,
in target order; failover targets after the first are dashed and numbered (`#2`).

Each arrow is labelled `x/y`, or just `x` when the pool is unlimited:

- **x** = requests of that route currently *active* on that target: they hold a slot in the pool and
  are being served by it, until the response (including an SSE stream) has ended, the client
  cancelled, or the attempt failed and the request moved to the next target.
  Requests still waiting in the queue are not counted; they show as a `queued` count on the
  route box and a badge on the pool.
- **y** = the pool's `max_concurrency` (the group's maximum for a concurrency group).

Arrows with x > 0 are highlighted and animated; a pool whose slots are all taken is filled
(saturated). Updates arrive every second while the view is visible and stop when it is hidden. Arrows
are keyboard-focusable with an `aria-label` such as
`fast to claude-haiku-5-5@low in claude-subscription: 1 of 3 active`.

`GET /api/state` exposes the per-link number as `endpoints[].targets[].in_flight`.

::: warning Per pod
With several proxy pods, pool slot counts and queues are shared through Valkey
([coordination](./coordination.md)), but the per route → model counters are kept per pod, and the
dashboard queries one pod. A coordinated pool is annotated "slots shared; links: this pod", and the
x values of its arrows can add up to less than the pool's shared active count. Counters reset on
restart. A hot reload that renames or removes a target drops its arrow; requests still running on it
finish without corrupting the counts.
:::

## Routing

| Request | Result |
| --- | --- |
| Host starts with `dashboard.` (or equals `dashboard.host`), any path | Dashboard (`/`, `/api/state`). Everything else is `404`, so the proxy API is **not** reachable through the dashboard host. Only `GET`/`HEAD`. |
| Any other host, path `/dashboard/...` | Dashboard. |
| Anything else (`/v1/...`, `/healthz`, `/status`) | Unchanged proxy behaviour. |

`/v1/`, `/healthz` and `/status` can never be shadowed: `dashboard.path` may not collide with them.

## Configuration

```yaml
dashboard:
  enabled: true        # default true
  host: ""             # exact hostname serving the dashboard at "/"; empty: any host starting with "dashboard."
  path: /dashboard     # prefix available on every host; "off" disables the path mount
```

The section is hot-reloaded like the rest of the manifest.

## Exposing it with Helm

```yaml
dashboard:
  ingress:
    enabled: true
    host: dashboard.llm-proxy.example.com
    # className / annotations / tls fall back to the `ingress.*` values when empty
```

This creates a second Ingress pointing at the same Service.

## Public by design

There is no authentication. The JSON (`GET /api/state`) and the HTML only contain endpoint/pool names,
upstream `host[:port]`, counts, priorities, timings and versions. API keys, expanded environment
variables, full upstream URLs (userinfo, path, query), request/response bodies, prompts, client
headers and reload error messages are never included; a test asserts it. Responses carry a strict
Content-Security-Policy. If endpoint names are sensitive, set `dashboard.enabled: false` or restrict the
hostname at your ingress.
