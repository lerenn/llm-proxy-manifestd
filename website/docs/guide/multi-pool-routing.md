# Multi-pool routing

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
