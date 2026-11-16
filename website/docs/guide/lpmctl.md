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

Or build it from a checkout: `go build -o lpmctl ./cmd/lpmctl`.

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

```sh
$ lpmctl status
Release proxy in namespace llm, pod proxy-llm-proxy-manifestd-6c9f7d-x2k4q (Running)

PROVIDER             TYPE           STATUS
claude-subscription  claude         logged-in
codex-subscription   codex          not logged-in
my-anthropic         anthropic-api  api-key

CLIENT    PRIORITY  ENDPOINTS
ci        10        fast
opencode  -         *
```

- **Sidecar providers** (`claude`, `codex`) are found from the pod spec: every container whose readiness probe runs `adapter-<type> ready`. lpmctl execs that same check in the container: exit 0 is `logged-in`, any other exit `not logged-in`. If the exec itself fails (container not running, no RBAC) the status is `unknown (<reason>)`.
- **API-key providers** (`anthropic-api`, `openai-api`) have no container and nothing to log in to; they are listed as `api-key`.
- **Clients** come from `clients` in the proxy ConfigMap's `config.yaml` (priority `-` means the route's priority, endpoints `*` means every route). Keys are not read by `status`.

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

API-key providers are refused (their key comes from a Secret). `login` needs a terminal on both stdin and stdout. The sidecar becomes Ready within one readiness period (10 s) after the login is stored; check with `lpmctl status`.

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
