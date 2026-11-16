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

- generates one random key per client (`llmp_` plus 40 random characters) in a Secret named `<release>-client-keys` on first install;
- reuses the existing values on `helm upgrade` through `lookup`, adds keys for new names and drops removed ones;
- mounts the Secret read-only into the proxy and the `check-config` init container, and renders `server.client_keys_dir` for you;
- keeps the Secret on uninstall (`helm.sh/resource-policy: keep`).

Read a key:

```sh
kubectl get secret <release>-llm-proxy-manifestd-client-keys -o jsonpath='{.data.opencode}' | base64 -d
```

(The Secret is named `<release>-client-keys` when the release name contains the chart name, otherwise `<release>-llm-proxy-manifestd-client-keys`; `kubectl get secret | grep client-keys` finds it.)

Rotate one key: delete that key from the Secret (`kubectl patch secret ... --type=json -p '[{"op":"remove","path":"/data/opencode"}]'`) and run `helm upgrade`; a new key is generated for that name.

### `helm template`, dry-run and Argo CD

`lookup` returns nothing when Helm does not talk to the cluster: `helm template`, `helm install --dry-run`, `helm lint` in CI, and **Argo CD** (it renders with `helm template`). In those cases every render invents new random keys, which is fine for CI rendering and the checker but would rotate your keys on every sync under Argo CD. For GitOps, create the Secret yourself (for example with SOPS) and point the chart at it:

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

- The `lookup` reuse path (keys surviving `helm upgrade`, adding and dropping names) was only checked by reading the template and rendering offline: I could not run it against a live cluster.
- The kubelet's Secret refresh timing and hot reload of the mounted keys were not exercised in a cluster (the reload logic is unit-tested against a directory).
- `endpoints` is an allow-list of endpoint names (protocol-agnostic: allowing `fast` allows both `endpoints.anthropic.fast` and `endpoints.openai.fast`).
