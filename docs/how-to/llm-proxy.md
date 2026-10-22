# Route model calls through llm-proxy

The llm-proxy service runs [LiteLLM](https://docs.litellm.ai) for your
sandboxes. It holds the model keys, and each sandbox gets a LiteLLM key of
its own, with a budget. So no gateway VM holds a model key, you see the spend
of each sandbox, and you can take one sandbox's access away without touching
the others. [Services](../concepts/services.md#llm-proxy) explains how the
keys travel.

```text
sandbox ── virtual key ──> llm-proxy VM: LiteLLM ── model key ──> api.anthropic.com
```

You need:

- a model key: an Anthropic API key, a Claude subscription token from
  `claude setup-token`, or the key of an OpenAI-compatible API;
- Podman on the host, with access to `docker.io` and PyPI: brig builds the
  LiteLLM image on the host at the first start and copies it into the VM.
  It pins LiteLLM 1.106.2, its packages by hash and its base images by
  digest, and moves them with brig releases.

## 1. Write the service's config directory

The service VM gets its own [config
directory](../concepts/openshell-config.md#config-directories). Do not use
one of your gateway VMs' directories: the service VM must hold its own keys
only.

```text
~/brig/models/
  profiles/anthropic-litellm.yaml   # where LiteLLM may send the key
  providers/anthropic.yaml          # where the key is in your keyring
  llm-proxy.yaml                    # the models that LiteLLM offers
  services/models.yaml              # LiteLLM's own two secrets
```

The profile lets LiteLLM's Python, by its real path, send the key to the
model API. This one is for an Anthropic API key:

```yaml
# profiles/anthropic-litellm.yaml
id: anthropic-litellm
display_name: Anthropic API for LiteLLM
description: Anthropic's API with an API key, for the llm-proxy
category: inference
inference_capable: true
credentials:
  - name: api_key
    description: An Anthropic API key
    env_vars: [ANTHROPIC_API_KEY]
    required: true
    auth_style: header
    header_name: x-api-key
discovery:
  credentials: [api_key]
endpoints:
  - host: api.anthropic.com
    port: 443
    protocol: rest
    access: read-write
    enforcement: enforce
binaries: [/usr/local/bin/python3.13]
```

```yaml
# providers/anthropic.yaml
name: anthropic
type: anthropic-litellm
credentials:
  ANTHROPIC_API_KEY:
    secret_tool:
      lookup: [service, anthropic.com, user, alice]
```

For a Claude subscription token, the credential is `ANTHROPIC_AUTH_TOKEN`,
sent as a bearer token. Use it in place of the API key, not next to it:

```yaml
# profiles/claude-litellm.yaml, changed from the one above
credentials:
  - name: oauth_token
    description: Claude subscription token from claude setup-token
    env_vars: [ANTHROPIC_AUTH_TOKEN]
    required: true
    auth_style: bearer
    header_name: authorization
discovery:
  credentials: [oauth_token]
```

Check that your plan's terms allow its use through a proxy.

`llm-proxy.yaml` lists LiteLLM's models and the limits of each sandbox's key:

```yaml
# llm-proxy.yaml
models:
  - model_name: claude-*              # what agents ask for
    litellm_params:
      model: anthropic/claude-*
      api_key: os.environ/ANTHROPIC_API_KEY
keys:                                 # each sandbox's key
  models: [claude-*]                  # empty: all models
  max_budget: 20                      # US dollars
  budget_duration: 30d                # then the spend starts again
```

An `api_key` can only be `os.environ/NAME`, where `NAME` is a credential of a
provider in the directory: LiteLLM's environment holds OpenShell's
placeholder, never the key. For the subscription token, leave `api_key` out.
`api_base` must be `https`. brig refuses settings that would hold a secret,
such as `aws_secret_access_key` or an `Authorization` in `extra_headers`.

LiteLLM needs two secrets of its own: the master key, with which brig makes
and deletes the sandboxes' keys and you log in to LiteLLM's UI, and the salt
key, which encrypts what LiteLLM keeps in its database. Make them and bind
them:

```sh
openssl rand -hex 32 | secret-tool store --label='llm-proxy master key' application brig service llm-proxy key master
openssl rand -hex 32 | secret-tool store --label='llm-proxy salt key' application brig service llm-proxy key salt
```

```yaml
# services/models.yaml
name: models
secrets:
  MASTER_KEY:
    secret_tool:
      lookup: [service, llm-proxy, key, master]
  SALT_KEY:
    secret_tool:
      lookup: [service, llm-proxy, key, salt]
```

Never change the salt key once the service has started: LiteLLM cannot read
its database with another one.

## 2. Create the service

Store the model key, then create the service:

```sh
secret-tool store --label='Anthropic API key (llm-proxy)' application brig service anthropic.com user alice
brig service create models --type llm-proxy --openshell-config ~/brig/models
```

At each start of its VM, brig:

1. syncs the directory to the VM's own gateway, so the model key is a
   provider there;
2. builds the LiteLLM image if the VM lacks it (some minutes, the first
   time), and creates the sandbox `litellm` from it, with the providers
   attached;
3. hands LiteLLM its config and its two secrets, in the sandbox's memory;
4. starts `brig-svc-forward-models.service`, a user unit on the host that
   serves the service's port and passes each connection through the VM's
   gateway to LiteLLM.

`brig service show models` prints the port. `brig sync models` hands LiteLLM
a changed `llm-proxy.yaml` or new secrets, and LiteLLM loads them again.

## 3. Point agents at it

With one llm-proxy service, `brig agent init` uses it by default, unless you
give `--endpoint` or `--anthropic`:

```sh
brig agent init claude --vm dev
brig agent init pi --vm dev --model claude-sonnet-4-5
```

The agent gets no model provider. Its image points at
`http://host.openshell.internal:PORT`, and `agent.yaml` lists the service:

```yaml
providers: []                       # what a new sandbox of this agent attaches
services: [models]                  # the brig services it is attached to
```

Claude Code finds the sandbox's key in `ANTHROPIC_AUTH_TOKEN`; with a
subscription token behind the proxy, its settings also name the OAuth beta in
`ANTHROPIC_BETAS`, as Anthropic wants. Pi and opencode find the key in
`LLM_PROXY_KEY`, and `--model` names a `model_name` of `llm-proxy.yaml`.

`brig sandbox create --service` attaches a new sandbox once it is ready, and
before `--connect` opens its shell:

```sh
brig sandbox create dev --connect --service models -- --name webapp --template claude
```

For a sandbox that runs already, attach it by hand, then start the agent from
a new shell:

```sh
brig service attach models dev webapp
```

## 4. Keys and budgets

`brig service attach` makes a LiteLLM key `brig/GATEWAY/WORKSPACE/SANDBOX`
with the limits of `keys:`, and stores it as the sandbox's provider; brig
keeps no copy. `brig service detach` deletes the key. Attaching again
replaces the key. At each start of the service, brig deletes the keys of
sandboxes that are no longer attached. Keys that you make in LiteLLM's UI
have other names, and brig leaves them alone.

A change of `keys:` applies to new keys. To give a sandbox the new limits,
detach and attach it, or change its key in LiteLLM's UI.

## 5. Keep model keys off the gateways

Once an llm-proxy service exists, `brig sync` and each start of a gateway VM
keep back every provider with a secret that the service holds too, and say
so:

```text
provider claude-code: kept off the gateway: service models holds its key
```

`brig sync dev --prune` deletes such providers that brig made earlier. A
provider whose key the proxy does not hold, such as a local model server's,
still reaches the gateway.

## LiteLLM's UI

Open `http://127.0.0.1:PORT/ui/` on the host, or "Open its UI" in the
services list of `brig ui`'s VMs page, and log in with the master key:
`secret-tool lookup application brig service llm-proxy key master`. It shows
the spend of each key and lets you change a key's budget. Gateways reach
the same port, but without the master key they get only what their own key
allows.

## When something fails

- `brig service show models` and `journalctl --user -u
  brig-svc-forward-models.service` show whether the host port is served.
- `brig sandbox exec models litellm -- tail /sandbox/postgres.log`, and
  `brig sandbox logs models litellm`, show LiteLLM and its database.
- `LiteLLM refused the master key`: the master key in your keyring is not
  the one LiteLLM started with. Run `brig sync models`.
- An agent gets 401 from the proxy: its sandbox has no key yet, or its
  shell started before the attach. Attach it, and start the agent from a new
  shell.
