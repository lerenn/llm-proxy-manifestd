# lpmctl (CLI)

`lpmctl` is a small command-line tool that runs **on your machine** and talks to the cluster with your kubeconfig, like `kubectl`. It reads a release's client keys, shows whether each subscription provider is logged in, and opens the interactive provider login. It is not part of any image and never runs in the pod.

## Install

Download the binary for your platform from the [GitHub releases](https://github.com/lerenn/llm-proxy-manifestd/releases) (assets `lpmctl-<os>-<arch>` for `linux`/`darwin` × `amd64`/`arm64`) and check it against `lpmctl-sha256sums.txt`:

```sh
os=$(uname -s | tr A-Z a-z); arch=$(uname -m); [ "$arch" = x86_64 ] && arch=amd64; [ "$arch" = aarch64 ] && arch=arm64
base=https://github.com/lerenn/llm-proxy-manifestd/releases/latest/download
curl -fsSLO "$base/lpmctl-$os-$arch" -O "$base/lpmctl-sha256sums.txt"
sha256sum --ignore-missing -c lpmctl-sha256sums.txt   # macOS: shasum -a 256 --ignore-missing -c lpmctl-sha256sums.txt
install -m 0755 "lpmctl-$os-$arch" ~/.local/bin/lpmctl
```

Or build it from a checkout: `go build -o lpmctl ./cmd/cli` (`go install github.com/lerenn/llm-proxy-manifestd/cmd/cli@latest` also works, but names the binary `cli`: rename it to `lpmctl`).

## Selecting the release

| Flag | Meaning |
| :- | :- |
| `-n`, `--namespace` | Namespace of the release (default: the namespace of the current kubeconfig context). |
| `--release` | Helm release name. When omitted, lpmctl looks for pods labelled `app.kubernetes.io/name=llm-proxy-manifestd` and uses their `app.kubernetes.io/instance`; this works only if the namespace holds exactly one release, otherwise it lists them and asks for `--release`. |
| `--context` | kubeconfig context. |
| `--kubeconfig` | kubeconfig file (default `$KUBECONFIG` or `~/.kube/config`). |

Auto-detection relies on the chart's labels, so it does not work with a `nameOverride`; pass `--release` there. The Secret and ConfigMap names are read from the running pod's volumes (so `clientsExistingSecret` and `fullnameOverride` are honoured); without a pod lpmctl falls back to the chart defaults (`<fullname>-client-keys`).

## `lpmctl keys [client]`

Reads the client keys Secret (see client keys in `config.clients`).

```sh
$ lpmctl keys
NAME      KEY
ci        llmp_…
opencode  llmp_…

$ lpmctl keys --hide        # same table, keys masked
$ lpmctl keys opencode      # only the key and a newline, nothing else
llmp_…
$ export ANTHROPIC_API_KEY="$(lpmctl keys opencode)"
```

With a client name the output is exactly the key, so it is safe to pipe or capture. An unknown client is an error on stderr with nothing on stdout. lpmctl never writes keys anywhere else (no logs, no files, no cache).

## `lpmctl status`

With `adapters.mode: separate` ([Rolling updates](/guide/rolling-updates)) the adapters run in their own pods (label `app.kubernetes.io/component=adapter`); `status` and `login` find them there and exec into them, so the output is the same as in sidecar mode.

```sh
$ lpmctl status
Release proxy in namespace llm, pod proxy-llm-proxy-manifestd-6c9f7d-x2k4q (Running)

PROVIDER             TYPE    STATUS
claude-subscription  claude  logged-in
codex-subscription   codex   not logged-in
nevermetered         api     anthropic,openai, max_concurrency 1 (no login needed)

CLIENT    PRIORITY  ENDPOINTS
ci        10        fast
opencode  -         *
```

- **Sidecar providers** (`claude`, `codex`) are found from the pod spec: every container with an `ADAPTER_TYPE` env var. lpmctl execs that same check in the container: exit 0 is `logged-in`, any other exit `not logged-in`. If the exec itself fails (container not running, no RBAC) the status is `unknown (<reason>)`.
- **API providers** (`type: api`, `anthropic-api`, `openai-api`) have no container and nothing to log in to. They are read from the pools of the ConfigMap's `config.yaml` (pools with an `api_key`): the pools of a `concurrency_group` (e.g. `nevermetered-anthropic` and `nevermetered-openai`) are shown on one line named after the group, with their protocols and `max_concurrency`, marked `no login needed`. If the ConfigMap is unreadable, they fall back to the proxy container's `PROVIDER_*_API_KEY` env.
- **Clients** come from `clients` in the proxy ConfigMap's `config.yaml` (priority `-` means the endpoint's own priority, endpoints `*` means every endpoint). Keys are not read by `status`.

## `lpmctl login <provider>`

Opens an interactive terminal (TTY, resizes forwarded) in the provider's sidecar container and runs the CLI login there; the login lands on the provider's PVC.

| Type | Command run | Headless flow |
| :- | :- | :- |
| `claude` | `claude` | type `/login`, pick the subscription, open the printed URL in a browser on your machine, paste the code back, then `/exit` |
| `codex` | `codex login --device-auth` | device code: open the printed URL on any device, sign in, enter the code |

```sh
lpmctl login claude-subscription
lpmctl login claude-subscription -- claude setup-token   # anything after -- replaces the command
```

API providers are refused with a clear message (no login needed; their key comes from a Secret). `login` needs a terminal on both stdin and stdout. The adapter starts accepting requests as soon as the login is stored (the pod is Ready regardless); check with `lpmctl status`.

## RBAC

lpmctl only uses your own credentials. The minimal Role in the release namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: lpmctl
  namespace: llm
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list]          # find the release and its pod
  - apiGroups: [""]
    resources: [pods/exec]
    verbs: [create, get]        # status (ready check) and login; get is used by the WebSocket exec
  - apiGroups: [""]
    resources: [configmaps]
    verbs: [get]                # status: clients and pool protocols
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get]                # keys only
```

| Command | Needs |
| :- | :- |
| `keys` | pods `get`/`list`, secrets `get` |
| `status` | pods `get`/`list`, pods/exec, configmaps `get` |
| `login` | pods `get`/`list`, pods/exec |

`pods/exec` on this pod gives access to the subscription logins on the PVCs, and `secrets get` to the client keys: grant them only to the people who operate the proxy. You can scope the Secret rule with `resourceNames: [<release>-llm-proxy-manifestd-client-keys]`.

## `lpmctl opencode`

opencode cannot discover the models of a custom provider, so this command generates them from the proxy's live `GET /v1/models` (once in OpenAI format with `Authorization: Bearer`, once in Anthropic format with `x-api-key` + `anthropic-version`) and rewrites the `models` map of two opencode providers.

```sh
export LPMCTL_PROXY_URL=https://llm-proxy.example.com/v1
lpmctl opencode --client opencode            # dry run (default): prints added/removed models, changes nothing
lpmctl opencode --client opencode --write    # apply
opencode service restart                     # opencode caches its config in a background service
```

| Flag | Meaning |
| :- | :- |
| `--url` | Proxy base URL including `/v1` (default `$LPMCTL_PROXY_URL`; no other default). |
| `--client` | Client whose key is used; optional when the release has exactly one client. |
| `--key-env VAR` | Read the key from an environment variable instead of the cluster. |
| `--config` | Config file (default `$OPENCODE_CONFIG`, else `$XDG_CONFIG_HOME/opencode/opencode.json`, else `~/.config/opencode/opencode.json`). |
| `--anthropic-provider`, `--openai-provider` | Provider ids (default `anyshiftlab-anthropic-proxy`, `anyshiftlab-openai-proxy`). |
| `--no-anthropic`, `--no-openai` | Skip one provider. |
| `--set-key` | Also write the key as the providers' `apiKey` (never printed). |
| `--write` | Apply. Atomic write, `<config>.bak` saved first if absent, file mode kept (`0600` for a new file). |

Only the `models` map of the two named providers is replaced: models you added by hand to them are removed, models removed from the proxy disappear, the settings of models that stay are kept. Every other key and provider is preserved (formatting is normalised to 2-space JSON). A missing provider block is created (`npm`, `name`, `options.baseURL`). Both the `provider` and the `providers` config styles are detected; a new file uses `provider`. If the file has comments or trailing commas it is not edited: nothing is written and the models are printed for you to paste. A 401/403 from the proxy is reported as a rejected key. Re-run it when the proxy's endpoints change.
