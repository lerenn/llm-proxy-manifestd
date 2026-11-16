# Dashboard

A lightweight, read-only web dashboard is embedded in the binary (plain HTML/CSS/JS from `ui/`, no
build step) and served by the proxy itself. It polls every 2 seconds and follows
`prefers-color-scheme`.

A left sidebar switches between three views; the current view is kept in the URL hash
(`#/overview`, `#/pools`, `#/endpoints`) so links and reloads work, and it defaults to Overview. On
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

The overall health badge stays visible in the header. Request and error counters are per process and
reset on restart.

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
