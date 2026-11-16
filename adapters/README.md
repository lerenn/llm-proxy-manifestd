# Adapters

Adapters are small OpenAI-compatible servers that front a vendor CLI. They run as
sidecar containers in the same pod as llm-proxy-manifestd, listen on
`127.0.0.1:<port>`, and are registered as a normal pool (`upstream_url:
http://localhost:<port>`). The proxy itself knows nothing about CLIs.

Contract: `POST /v1/chat/completions` (SSE streaming and non-stream),
`GET /ready` (200 only when logged in). A model string may end in
`@low|@medium|@high`; the adapter strips it and passes it as the CLI effort.

## claude

`adapters/claude` spawns the official `claude` CLI once per request
(`-p --input-format stream-json --output-format stream-json
--include-partial-messages --tools "" --no-session-persistence`) and
translates its events to OpenAI chunks. Login state lives in
`CLAUDE_CONFIG_DIR` (default `/data/claude`, meant to be a PVC). Image:
`ghcr.io/lerenn/llm-proxy-manifestd-adapter-claude`; the `claude` native binary
is pinned and checksum-verified in the Dockerfile (no Node.js in the image).

Environment: `CLAUDE_CONFIG_DIR`, `ADAPTER_LISTEN` (default `127.0.0.1:9100`),
`CLAUDE_BIN`.

Status: tested only against a fake CLI that emits the documented stream-json
events. It has never run against a real subscription login.

## codex

Not implemented. See the docs site for the design note.
