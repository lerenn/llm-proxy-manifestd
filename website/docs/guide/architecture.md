# Architecture

```text
client ──► /v1/* ──► endpoint lookup ─► pool.Acquire(priority) ──► ReverseProxy ──► upstream
                         │                      │ (queue_timeout)        │ (FlushInterval: -1)
               target + model rewrite      slot held until the stream ends
```

## Request flow

1. The body is buffered and the JSON `model` is looked up in `endpoints.<protocol>`, the protocol
   being `anthropic` for `/v1/messages` and `openai` for other paths.
2. A target is chosen per the endpoint `mode`; `model` is rewritten to its `target_model` (if set)
   and `Content-Length` is corrected.
3. A slot is acquired from the target's pool, bounded by the client context and `server.queue_timeout`.
4. The request is forwarded with `Authorization: Bearer <api_key>`; headers such as `X-Session-Id` and
   `OpenAI-Beta` are preserved. `X-Priority` is consumed by the proxy.
5. The response is streamed with flushing after every write. The slot is released **only after the body
   has been fully copied** (or the client disconnects), not when headers arrive.

## Runtime state and hot reload

The active configuration and pools live in one immutable `RuntimeState` behind an
`atomic.Pointer`. Each request loads it exactly once.

Changes are detected by watching the **parent directory** of the config file (Kubernetes updates
ConfigMap volumes by swapping the `..data` symlink) and on `SIGHUP`. A reload:

- is rejected, with an error log, if the new manifest is invalid; the previous state stays active;
- **reuses the existing pool** (same semaphore and queue) when `upstream_url` and `api_key` are
  unchanged. A new `max_concurrency` or `aging_interval` is applied live: raising the limit
  immediately admits queued waiters, lowering it never kills running streams, it just stops new
  grants until active slots drop below the limit;
- creates a **new pool** when `upstream_url` or `api_key` changed. Requests already in flight release
  on the pool instance they acquired from, so there is no panic and no leaked slot;
- ignores `server.listen` changes (restart required).

## Graceful shutdown

On `SIGTERM`/`SIGINT` the server stops accepting connections and waits for in-flight requests
(`--shutdown-timeout`, default 10 minutes). Combine with `terminationGracePeriodSeconds: 600`.

## Flags

| Flag | Default | |
| --- | --- | --- |
| `--config` | `/etc/llm-proxy-manifestd/config.yaml` | Manifest path. |
| `--shutdown-timeout` | `10m` | Max time to drain on shutdown. |
| `--version` | | Print the version. |
