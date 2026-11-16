# Multi-pool routing

An endpoint can spread requests over several targets, for example several accounts of the same provider, or a cheaper model as a fallback. Each target names one `pool` and the `target_model` to send to it; all pools of an endpoint must speak the endpoint's protocol (see [Protocol endpoints](./protocol-endpoints)).

```yaml
pools:
  claude-a: {upstream_url: http://localhost:9100, protocol: anthropic, max_concurrency: 2}
  claude-b: {upstream_url: http://localhost:9101, protocol: anthropic, max_concurrency: 2}
  codex-sub: {upstream_url: http://bridge:8317, api_key: "${KEY}", max_concurrency: 2}
endpoints:
  anthropic:
    standard-medium:
      mode: least-inflight            # default
      priority: 50
      targets:
        - {pool: claude-a, target_model: "claude-sonnet-5-5@medium"}
        - {pool: claude-b, target_model: "claude-sonnet-5-5@medium"}
    premium-high:
      mode: failover
      priority: 80
      targets:
        - {pool: claude-a, target_model: "claude-opus-5-5@high"}
        - {pool: claude-b, target_model: "claude-sonnet-5-5@high", priority: 40}   # backup, lower queue priority
  openai:
    codex-default:
      targets: [{pool: codex-sub, target_model: gpt-5.4}]   # single target, priority 0
```

## Modes

`mode` decides which target an attempt uses. In every mode, a target is only taken among those **not yet tried** for this request (see failover below).

| Mode | Free slot available | All candidate pools full |
| --- | --- | --- |
| `least-inflight` (default) | The target whose pool has the fewest in-flight requests (held slots) and a free slot; ties go to the target listed first. | Queue on the pool with the lowest in-flight plus queued count; ties go to the target listed first. |
| `round-robin` | Each request starts one target further in the list (a per-endpoint cursor); the first target in that rotated order with a free slot wins. | Queue on the rotation's first target. |
| `failover` | Always the first remaining target, in list order. | Queue on that same target: later targets are **not** used to absorb load, only when it fails. |

Once queued, a request stays on that pool until it gets a slot or `server.queue_timeout` expires (then `503 queue_timeout`).

## Queue priority

A request queues with, from strongest to weakest: the `X-Priority` request header, the chosen target's `priority`, the endpoint's `priority`, else `0`.

## Failover (all modes)

1. If the chosen target fails with a transport error (connect failure, reset before response headers) or answers 5xx, the proxy releases the slot and retries the same request on another target not yet tried, chosen by the same mode rules.
2. 4xx responses are never retried. Failover only happens before any response byte reaches the client: once headers are forwarded (streaming started), a later upstream failure is not retried.
3. If every target failed, the last attempt's result is returned (its 5xx response, or `502 upstream_error` for a transport error).
4. `X-Pool-Name` reports the pool that finally served the request. The body sent to each target carries that target's `target_model`.

Config changes, including an endpoint's targets and mode, are applied on hot-reload; in-flight requests finish on the pools they started with. Pools whose upstream is unchanged keep their slots and queues across reloads.

`target_model` is an opaque string passed upstream unchanged. A suffix such as `@low`, `@medium` or `@high` (e.g. `claude-sonnet-5-5@medium`) has no meaning to the proxy; interpreting it is up to the upstream.

The dashboard shows each endpoint's mode and targets with their upstream host names.
