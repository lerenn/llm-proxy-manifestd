# Coordination (shared limiter, Valkey)

By default every proxy pod keeps its slots and priority queue in memory. That is exact with a
single pod, but during a rolling update with surge two pods run at once and each would grant its
own `max_concurrency`. The optional `coordination` block shares **slot limits and the priority
queue** between pods through a Valkey (or any Redis-protocol) server.

Without the block, behaviour is exactly the in-memory one. Pools with unlimited concurrency
(`max_concurrency: 0`) never touch Valkey.

## Configuration

```yaml
coordination:
  valkey:
    address: "valkey:6379"        # required
    password_env: "VALKEY_PASSWORD" # optional: name of the env var holding the password
    key_prefix: "lpm"             # default
    lease_ttl: 30s                # default; slot lease TTL, renewed while the request runs
    fail_mode: open               # open (default) | closed
```

The password is never put in the manifest: `password_env` only names the environment variable.
Changing the block needs a restart (it is ignored, with a warning, on hot reload). `checker`
validates the block and warns when `lease_ttl` is below `3s`.

## How it works

Each pool (or each [concurrency group](/guide/subscription-providers#concurrency-groups-proxy-config),
keyed by the group) has, in Valkey, a sorted set of waiters, a sorted set of live leases and the
stored limit.

- **Ordering.** Aging is linear, so the order `priority + waited/aging_interval` can be stored as a
  static score (`enqueue_time/aging_interval - priority`), computed once with the Valkey `TIME`
  (never a pod clock) when the request enqueues. [Priority and aging](/guide/priority-queues)
  semantics are unchanged, including `X-Priority`.
- **Taking a slot** is one atomic Lua script: purge expired leases and stale waiters, check the
  caller is at the head of the queue (or the queue is empty) and that live leases are below
  `max_concurrency`, then add a lease and remove the waiter.
- **Waiting.** Waiters retry about every 30 ms; each retry also refreshes the waiter's liveness, so
  a crashed pod's waiters are purged after `lease_ttl`. A client disconnect or `queue_timeout`
  removes the waiter at once, with the same `503 queue_timeout` as before.
- **Leases.** A lease is renewed every `lease_ttl/3` while the request (including the SSE stream)
  runs and removed on completion. If a pod crashes, its leases expire after `lease_ttl`.
- **Status.** `/status` and the dashboard read slots in use and queue depth from Valkey; the pool is
  marked `coordinated: true`. `total_served` stays per pod.

## Failure modes

`fail_mode: open` (default): when Valkey is unreachable the pod logs a warning and falls back to
its local in-memory pool, so limits are per pod until Valkey is back (retried every 2 s).
`fail_mode: closed`: requests are rejected with `503` and code `coordination_unavailable`.

If Valkey **restarts without persistence**, leases and the queue are lost. Queued requests notice
their entry is gone and re-queue (at the back); requests already running keep going but are no
longer counted until they finish, so the limit can briefly be exceeded (a short double-slot
window). Nothing deadlocks.

## Rollouts and compatibility

During a rollout, old (in-memory) and new (coordinated) proxies do not see each other: the old pod
counts locally. Roll out the first coordinated version with `maxSurge: 0`, or accept one window of
double limits. Between coordinated versions the key layout (`{prefix:pool:NAME}:q|l|w|max`) and
Lua scripts are stable; a change would use a new key prefix. See also the
Rolling updates guide (added with the chart's surge support).

## Mixed `max_concurrency`

Each pod reads the limit from its own config and publishes it to Valkey. Different values across
pods are an operator mistake: a pod that sees a different stored limit logs a warning (at most every
5 minutes per pool). A hot reload that resizes `max_concurrency` applies to the shared limit as soon
as every pod has reloaded.
