# Services

A service is a workload that agents in sandboxes may use but never control,
such as a model proxy that holds the API key, or a controller that runs
VMs for them. Each sandbox that may use a service gets a token of its own,
so the service knows which gateway, workspace and sandbox calls it, and
brig can take one sandbox's access away without touching the others.

## Types

A service's type decides what runs and where. Types are built into brig;
`brig service types` lists them. A type says:

- its **kind**: a *broker* holds a secret or a power and offers agents a
  narrow API; a *runner* runs the agents' workloads and holds no secrets;
- its **placement**: most services run in a VM of their own, which brig
  makes from the base image like a gateway VM, with or without OpenShell;
  a controller that must create VMs runs on the host as a user unit;
- the **programs** in a sandbox that may call it, such as `/usr/bin/node`.
  OpenShell adds the token only to their requests.

## How a sandbox reaches a service

```text
sandbox ── http://host.openshell.internal:PORT ──> relay in the gateway VM
        ──> 127.0.0.1:PORT on the host ──> the service (VM or host)
```

- brig picks `PORT` on the host's loopback interface when it creates the
  service. It stays the same for the service's life.
- In a sandbox, OpenShell maps `host.openshell.internal` to the gateway
  VM's own loopback interface. brig runs a relay there, a socket-activated
  user unit `brig-svc-NAME`, which passes connections on to the host.
- The gateway VM's [firewall](architecture.md#network-profiles-in-detail)
  lets it reach the ports of the services that its sandboxes are attached
  to, even when its network profile blocks the host. Attach and detach
  change it at once.

A type whose workload runs in an OpenShell sandbox of its VM, such as
llm-proxy, listens on the sandbox's loopback interface, which nothing in the
VM's network reaches. brig then forwards nothing into the VM. A user unit on
the host, `brig-svc-forward-NAME`, serves the host port instead and passes
each connection through the service VM's OpenShell gateway to the sandbox:

```text
127.0.0.1:PORT on the host ──> brig-svc-forward-NAME
        ──> the service VM's gateway (mTLS) ──> 127.0.0.1:4000 in the sandbox
```

## Tokens

`brig service attach mirror dev webapp` gives sandbox `webapp` of VM `dev`
a token for service `mirror`:

1. brig mints the token and records its SHA-256 hash, never the token, in
   `~/.local/share/brig/services/mirror/tokens.json`, which the service
   checks callers against.
2. brig stores the token on the gateway as provider
   `brig-svc-mirror-webapp`, with a profile `brig-svc-mirror` that it
   generates, and attaches the provider to the sandbox.
3. Programs that the sandbox starts afterwards see a
   [placeholder](openshell-config.md) in the variables that `brig service
   show mirror` lists; OpenShell puts the token in its place on the way out.

`brig service detach` revokes the token at once and removes the provider.
Deleting a sandbox with `brig sandbox delete`, or its VM with `brig delete`,
revokes its tokens too. Each start of a gateway VM, and `brig sync`, revoke
the tokens of sandboxes that are gone, give a new token to a sandbox whose
provider is gone, and delete the `brig-svc-` providers and profiles that no
service needs. Config directories cannot use names that start with
`brig-svc-`.

## Secrets of a service

A service VM holds only its own secrets: its config directories are the
ones that `brig service create --openshell-config` names, never
`openshell.configs` from `config.yaml`. Bind the secrets that its type
needs in `services/NAME.yaml` of such a directory, as a provider binds its
credentials:

```yaml
name: mirror
secrets:
  UPSTREAM_TOKEN:
    secret_tool:
      lookup: [service, mirror.example.org]
```

At every start of the service VM, brig looks the secrets up in your keyring,
copies them over SSH into `/run/brig/service/secrets/` in the VM's memory
and starts the workload. They never reach a disk. `brig sync mirror` hands
them over again and restarts the workload.

## llm-proxy

The llm-proxy type runs LiteLLM and its Postgres database in the sandbox
`litellm` of its VM. Its tokens are LiteLLM's virtual keys: LiteLLM makes
and checks them, so brig keeps no hash. A model call passes two
[placeholders](openshell-config.md), and each one is swapped once:

```text
agent in sandbox webapp, gateway VM dev
  │  Authorization: Bearer <placeholder of the virtual key>
  ▼
dev's OpenShell proxy: swaps in the virtual key brig/dev/default/webapp
  │  http://host.openshell.internal:PORT
  ▼
host port ──> brig-svc-forward-models ──> LiteLLM in the sandbox litellm
  │  checks the key, its models and its budget, records the spend
  │  x-api-key: <placeholder of the model key>
  ▼
the service VM's OpenShell proxy: swaps in the model key
  │
  ▼
api.anthropic.com
```

- No gateway VM holds a model key, and no sandbox holds a key at all: an
  agent that prints its environment shows two placeholders.
- LiteLLM sees only the model key's placeholder, so a LiteLLM that is
  broken into cannot send the key elsewhere: the service VM's proxy adds it
  only to requests from LiteLLM's Python to the hosts of the key's profile.
- LiteLLM's master key and salt key are service secrets: brig hands them to
  LiteLLM at every start, in the sandbox's memory. The master key gives
  power over LiteLLM, not over the model keys.
- Once an llm-proxy service exists, `brig sync` keeps the providers whose
  secrets it holds off the gateways.

[Route model calls through llm-proxy](../how-to/llm-proxy.md) sets one up.

## Commands

| Command | What it does |
| --- | --- |
| `brig service create NAME --type TYPE` | Create and start a service. |
| `brig service list`, `show NAME` | Show services, their URLs and attached sandboxes. |
| `brig service attach SERVICE [VM] SANDBOX` | Give a sandbox a token. |
| `brig service detach SERVICE [VM] SANDBOX` | Revoke it. |
| `brig service start NAME`, `stop NAME` | Start or stop a service. |
| `brig service delete NAME` | Revoke all tokens and delete the service. |

A service VM is a VM like others for `brig start`, `stop`, `show`, `ssh`,
`update` and `sync`. `brig list` leaves service VMs out, `brig ui` lists the
services under the VMs, with a link to a service's own UI, and `brig delete`
refuses them.
