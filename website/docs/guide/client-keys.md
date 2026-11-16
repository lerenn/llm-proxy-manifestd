# Client API keys

By default the proxy accepts any caller. Define `clients` in the manifest to require an API key on every `/v1/` request (and on `/status`). Each client gets its own key, so you can tell callers apart in the logs and on the dashboard and revoke one without touching the others.

```yaml
config:
  clients:
    opencode: {}                        # no settings (use {} in Helm values, not null: Helm drops null keys)
    ci: {priority: 10, endpoints: [fast]}
```

| Field | Meaning |
| :- | :- |
| (client name) | Also the name of the key file. Letters, digits, `.`, `_`, `-`; must start with a letter or digit. |
| `priority` | Optional default priority for this client's requests. The `X-Priority` header still overrides it. Without it the endpoint's (and per-target) priorities apply; when set, it replaces them for every target, like `X-Priority`. |
| `endpoints` | Optional allow-list of **endpoint names** (the request's `model`, i.e. names under `endpoints.anthropic` / `endpoints.openai`; a name allows it under both protocols). Omitted means every endpoint. Other endpoints answer 403 `model_not_allowed` and are hidden from `GET /v1/models`. |

Empty or omitted `clients` means authentication is disabled (the previous behaviour).

## How keys are presented and checked

- Anthropic clients send `x-api-key: <key>`, OpenAI clients `Authorization: Bearer <key>`.
- Missing or wrong key: `401`, in the protocol's error shape (OpenAI `error.code: invalid_api_key`, Anthropic `type: authentication_error`).
- `/healthz` stays open. The dashboard (served on its own host) is unchanged; it shows client names and per-client request counts, never keys.
- The manifest only holds client **names**. The keys are files in a directory (`server.client_keys_dir`, a mounted Secret: file name = client name). The proxy keeps only the sha256 of each key and compares hashes in constant time.
- A client without a key file is rejected (every request gets 401) and `checker` warns about it. A key file without a client in the manifest is ignored.
- The directory is re-read every 5 seconds, so adding or rotating a key takes effect once the kubelet has refreshed the mounted Secret (typically up to a minute), without a restart. Changes to `clients` itself hot-reload like the rest of the manifest.
- Each authenticated request logs the client name (`request` line); keys are never logged.

## Helm

With the chart, set `config.clients` as above. The chart:

- runs a `keys-manager` init container (same image, binary `/keys-manager init`) before the proxy starts. It creates the Secret `<release>-client-keys` if absent and adds one random key (`llmp_` plus 40 random characters) for each client that has none. It never overwrites an existing key, drops the keys of clients removed from `config.clients`, retries on update conflicts and is idempotent. It logs client names only, never keys;
- creates a ServiceAccount, a Role (`create` on secrets, plus `get`/`update`/`patch` limited by `resourceNames` to that one Secret) and a RoleBinding. The API token is mounted **only** in the init container (projected volume); the proxy and adapter containers have no Kubernetes API token;
- mounts the Secret read-only (the volume is `optional`, since the Secret does not exist before the init container's first run on a fresh install; until the kubelet projects it, the proxy has no keys and rejects every client) into the proxy and the `check-config` init container, and renders `server.client_keys_dir` for you;
- does not render the Secret itself: it is not a Helm/Argo CD object, so it is never pruned, diffed or regenerated, and it survives uninstall.

A client added to `config.clients` later gets its key at the **next pod restart** (the init container only runs then): `kubectl rollout restart deploy/<release>-llm-proxy-manifestd`.

Read a key:

```sh
kubectl get secret <release>-llm-proxy-manifestd-client-keys -o jsonpath='{.data.opencode}' | base64 -d
```

(The Secret is named `<release>-client-keys` when the release name contains the chart name, otherwise `<release>-llm-proxy-manifestd-client-keys`; `kubectl get secret | grep client-keys` finds it.)

Rotate one key: delete that key from the Secret (`kubectl patch secret ... --type=json -p '[{"op":"remove","path":"/data/opencode"}]'`) and restart the pod; a new key is generated for that name.

### GitOps / Argo CD / `helm template`

Because the keys are created at runtime by the init container (not by Helm `lookup`), `helm template`, `--dry-run` and Argo CD work without regenerating keys. If you prefer to own the Secret yourself (for example with SOPS), set `clientsExistingSecret`: the init container and the RBAC are then not rendered.

```yaml
clientsExistingSecret: my-client-keys   # keys of the Secret = client names
config:
  clients:
    opencode: {}
```

```sh
kubectl create secret generic my-client-keys --from-literal=opencode="llmp_$(openssl rand -hex 24)"
```

## Unverified

- The `keys-manager` init container is unit-tested against a fake API server and the chart rendering is tested offline; it was not run against a real cluster (RBAC, projected token, in-cluster CA).
- The kubelet's Secret refresh timing and hot reload of the mounted keys were not exercised in a cluster (the reload logic is unit-tested against a directory).
- `endpoints` is an allow-list of endpoint names (protocol-agnostic: allowing `fast` allows both `endpoints.anthropic.fast` and `endpoints.openai.fast`).
