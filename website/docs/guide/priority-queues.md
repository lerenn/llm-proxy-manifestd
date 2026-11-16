# Priority Queues

Each pool has `max_concurrency` slots. When all are busy, requests wait in a priority queue.

::: warning Default changed: unlimited
`max_concurrency: 0`, or omitting it, means **unlimited**: no slot limit, no queue, requests go straight through (so priorities and aging do not apply to that pool). Previously `max_concurrency` was required and had to be at least 1. Set an explicit positive number to keep a limit. Negative values are rejected. Switching a live pool between limited and unlimited on hot-reload is supported: waiters queued under a limit are released immediately when it becomes unlimited, and a new limit applies to new requests only (slots already held are kept). The dashboard shows `unlimited` instead of a slot count.
:::

## Ordering

1. Higher **effective priority** first.
2. Ties are served in strict FIFO order (earliest arrival first).

An endpoint's `priority` (default `0`) is the base priority; a target's own `priority`, when set,
replaces it for requests queued on that target. A request can override both with the
`X-Priority: <int>` header.

## Aging

With `server.aging_interval: "5s"`, a waiting request gains **+1 priority per interval waited**:

```text
effective = base + floor(time_waited / aging_interval)
```

So a priority-10 background job that has waited 60 s outranks a freshly arrived priority-15 request
(10 + 12 = 22), and low-priority work cannot starve. Leave it unset to disable aging. Because effective
priority depends on time, the queue is re-evaluated at the moment a slot is released.

With several proxy pods (rolling updates), the queue can be shared: see [Coordination](/guide/coordination).

## Direct handoff

When a slot is released and someone is waiting, it is handed **directly** to the best waiter: the
active count never drops, so a new arrival cannot barge in between.

## Cancellation and timeouts

- If a client disconnects while queued, it leaves the queue immediately.
- If it was handed a slot at the same moment, the slot is given back; nothing leaks.
- After `server.queue_timeout` the client gets a `503` with code `queue_timeout`.

## Example

```yaml
endpoints:
  openai:
    codex-interactive: {priority: 100, targets: [{pool: codex-sub, target_model: gpt-5.4}]}
    codex-default:     {priority: 50,  targets: [{pool: codex-sub, target_model: gpt-5.4}]}
    codex-background:  {priority: 10,  targets: [{pool: codex-sub, target_model: gpt-5.4}]}
```

With `max_concurrency: 2`, two background agents can use the account; an interactive session arriving
later takes the next freed slot ahead of any queued background work.

Check the live state with `GET /status`.
