# Agents

[Your first agent](../start/first-agent.md) has `brig agent init` write an
agent's files. This page shows those files for Claude Code, Pi and opencode,
explains the choices behind them, and is the manual path for building your
own image. Edit the files freely and run `brig agent build dev NAME` after
each change, then `brig agent update dev NAME` to move the agent's sandboxes to
the new image with their `/sandbox`. Examples use the VM `dev`, the sandbox `webapp` for the
repository `alice/webapp` and the shared [config
directory](../concepts/openshell-config.md#config-directories)
`~/src/agent-openshell`.

The three images follow one pattern: what you curate (the agent, tools, your
instructions and extensions) goes into the image, so a new image brings new
versions; what the agent writes (settings, sessions, checkouts) goes under
`/sandbox`, the only directory that is writable and persists.

## Pi

[Pi](https://pi.dev) has no official image, so you build one. Everything you
curate (Pi, tools, your extensions and skills) goes into the image under
`/usr/local`; everything Pi writes (settings, sessions) goes under `/sandbox`.
The default policy lets a sandbox read `/usr` and write only `/sandbox` and
`/tmp`, and only `/sandbox` persists.

| What | Where | Why |
| --- | --- | --- |
| Node, Pi, git, gh, glab, ripgrep, fd | image, `/usr/local` and `/usr/bin` | readable by default; updated by rebuilding the image |
| Your extensions, skills, prompts, themes | image, `/usr/local/share/pi-kit` (a Pi package) | loaded by path, so a new image brings new versions |
| Third-party Pi packages | image, `npm install -g` at build time | the sandbox cannot reach npm or GitHub at run time |
| Your model endpoint and its models | image, `/usr/local/etc/pi/models.json` | linked into Pi's agent dir, so a new image brings a new model list |
| Pi's agent dir: `settings.json`, `sessions/` | `/sandbox/.pi/agent` via `PI_CODING_AGENT_DIR` | writable and persistent; `~` is not writable |
| Project checkouts | `/sandbox/<repo>` | same workspace volume |
| Plugins under development | host directory, mounted read-only | edit on the host, `/reload` in Pi |

### The image

```text
pi-image/
  Containerfile
  pi-sandbox                  # start script, below
  etc/settings.json           # Pi settings for a fresh sandbox
  etc/models.json             # your model endpoint, see Model endpoint
  etc/AGENTS.md               # your standing instructions
  pi-kit/                     # optional: your Pi package, see the Containerfile
    package.json
    extensions/  skills/  prompts/  themes/
```

<!-- agent-file: pi agents/pi/Containerfile -->
```dockerfile
# Containerfile
FROM registry.fedoraproject.org/fedora-minimal:44

ARG PI_VERSION=1.1.0
RUN dnf -y install --setopt=install_weak_deps=False \
      fd-find gh git-core jq nodejs24 nodejs24-bin nodejs24-full-i18n \
      nodejs24-npm-bin ripgrep tar \
 && dnf clean all \
 && useradd --create-home --uid 1000 pi
# glab: add it to the list if your Fedora release packages it, or install the
# .rpm from its releases page, at /usr/bin/glab
# npm's global installs go to /usr/local, where the paths below expect them.
ENV NPM_CONFIG_PREFIX=/usr/local

RUN npm install -g --ignore-scripts "@earendil-works/pi-coding-agent@${PI_VERSION}"
# Third-party Pi packages, pinned. --legacy-peer-deps keeps npm from
# installing a second copy of Pi, which Pi's packages must not bundle. List
# each in "packages" in etc/settings.json by its path under
# /usr/local/lib/node_modules.
# RUN npm install -g --ignore-scripts --legacy-peer-deps @example/pi-tools@1.0.0

# Your own Pi package (extensions, skills, prompts, themes): create pi-kit/
# next to this file, list /usr/local/share/pi-kit in "packages" in
# etc/settings.json and uncomment these lines.
# COPY pi-kit/ /usr/local/share/pi-kit/
# RUN cd /usr/local/share/pi-kit \
#  && npm install --omit=dev --ignore-scripts --legacy-peer-deps
COPY etc/ /usr/local/etc/pi/
COPY --chmod=755 pi-sandbox /usr/local/bin/pi-sandbox

RUN git config --system credential.https://github.com.helper \
      '!/usr/bin/gh auth git-credential'

USER pi
WORKDIR /sandbox
ENV PI_CODING_AGENT_DIR=/sandbox/.pi/agent \
    GH_CONFIG_DIR=/sandbox/.config/gh \
    GIT_CONFIG_GLOBAL=/sandbox/.gitconfig \
    PI_OFFLINE=1 PI_SKIP_VERSION_CHECK=1 PI_TELEMETRY=0
```

The image builds on Fedora minimal, like the VMs: glibc, so whatever an agent
installs or downloads runs, and current packages, such as Node 24. `fd` and
`ripgrep` are in the image because Pi otherwise downloads them on first use,
which the sandbox blocks. `nodejs24-full-i18n` holds the Unicode data that Pi's
terminal UI needs: without it, Node crashes on `Intl.Segmenter`, and Pi dies
with a segmentation fault (exit 139) right after it starts, leaving mouse
tracking on and flooding the shell with the terminal's replies to Pi's colour
queries. The `PI_*` switches stop Pi's own update
checks and telemetry, which would only show up as denials. Do not put files
under `/sandbox` in the image: Podman copies them into a sandbox's workspace
once, at creation, and image updates never reach them again. A settings file
that the start script seeds only once behaves the same; `brig agent update dev
NAME --reset PATH` leaves PATH out of the move, so that the new image's start
script sets it up fresh.

The start script copies Pi's settings into a fresh sandbox (copied, not
linked, because Pi writes to them) and links what the image keeps managing:

<!-- agent-file: pi agents/pi/pi-sandbox -->
```sh
#!/bin/sh
# pi-sandbox: start Pi with this image's settings, extensions and skills
set -e
mkdir -p "$PI_CODING_AGENT_DIR"
[ -e "$PI_CODING_AGENT_DIR/settings.json" ] ||
  cp /usr/local/etc/pi/settings.json "$PI_CODING_AGENT_DIR/settings.json"
ln -sf /usr/local/etc/pi/AGENTS.md "$PI_CODING_AGENT_DIR/AGENTS.md"
ln -sf /usr/local/etc/pi/models.json "$PI_CODING_AGENT_DIR/models.json"
exec pi "$@"
```

`etc/settings.json` lists packages by absolute path, so they load in place and
a newer image brings new code. It starts empty: add `/usr/local/share/pi-kit`
for your own package, and the path under `/usr/local/lib/node_modules` of each
third-party one.

<!-- agent-file: pi agents/pi/etc/settings.json -->
```json
{
  "packages": [],
  "defaultProvider": "llm",
  "defaultModel": "your-model-id"
}
```

Build with Podman on the host and copy the image into the VM, tagged by date
so you can tell images apart and go back:

```sh
podman build -t localhost/pi-agent:2026-10-08 pi-image/
brig image push dev localhost/pi-agent:2026-10-08
```

### Model endpoint

Pi talks to any endpoint that speaks OpenAI's chat completions API: most
hosted services, such as OpenRouter, OpenAI, NVIDIA NIM, DeepInfra, Together
and Groq, and self-hosted servers such as vLLM, LiteLLM and Ollama. One profile,
one provider and one `models.json` entry connect it. The profile is your own,
in the shared directory:

<!-- agent-file: pi profiles/llm.yaml -->
```yaml
# profiles/llm.yaml
id: llm
display_name: LLM endpoint
description: An OpenAI-compatible inference endpoint
category: inference
inference_capable: true
credentials:
  - name: api_key
    description: The endpoint's API key
    env_vars: [LLM_API_KEY]
    required: true
    auth_style: bearer
    header_name: authorization
discovery:
  credentials: [api_key]
endpoints:
  - host: llm.example.com         # the host in baseUrl
    port: 443
    protocol: rest
    access: read-write
    enforcement: enforce
# Pi's node by its real path, whichever it is, and opencode's own binary
binaries: [/usr/bin/node, /usr/bin/node-*, /usr/local/bin/opencode]
```

The provider file looks up `LLM_API_KEY` as `service llm.example.com`, like
the GitHub one in [config
directories](../concepts/openshell-config.md#config-directories). `etc/models.json` tells Pi where the endpoint is and
which models it serves:

<!-- agent-file: pi agents/pi/etc/models.json -->
```json
{
  "providers": {
    "llm": {
      "baseUrl": "https://llm.example.com/v1",
      "api": "openai-completions",
      "apiKey": "$LLM_API_KEY",
      "models": [{ "id": "your-model-id" }]
    }
  }
}
```

Pi sends the placeholder in `LLM_API_KEY` as a bearer token, and the proxy
swaps in your key on requests to the profile's host. The profile admits only
`node` there (and opencode, which can share it); a rule that lets other programs reach that host lets them use the
key too. To change endpoints, change the host in both files. A server on your
LAN or on the host also needs its port in the profile and a [network
profile](../reference/network-profiles.md) for the VM that allows it, such as
`ollama`. For a service that Pi knows by name, such as OpenRouter, you can drop
`models.json` and use the service's own variable (`OPENROUTER_API_KEY`) in the
profile, so that Pi offers its built-in model list. Do not run Pi's `/login` in
a sandbox: it would store a real token in `auth.json` there.

### Start it

```sh
brig sync dev                     # applies the profiles and providers above
brig use dev
openshell sandbox create --name webapp \
  --from localhost/pi-agent:2026-10-08 \
  --provider llm --provider github
# now in a login shell inside the sandbox
git clone https://github.com/alice/webapp.git && cd webapp
pi-sandbox
```

Clone with the `.git` suffix: git uses the URL as given, and the push rules
[in Git access](git-access.md#grant-push-and-pull-requests-for-one-repository) match it.

Without a trailing command, the sandbox's main process is a login shell and
the sandbox stays after you leave. Press `Ctrl-P`, `Ctrl-Q` to detach with Pi
still working; `brig sandbox connect dev webapp` reattaches. For a second
shell, run `brig sandbox exec dev webapp --tty -- bash -l`. These
commands run your host's `ssh`, which reads `~/.ssh/config`: make sure no
`Host *` block there turns on `ForwardAgent` or other forwarding, since the
other end is the VM.

A template saves the image, environment, resources and driver config under a
name: create it once with `openshell sandbox template create pi --image
localhost/pi-agent:2026-10-08`, then `openshell sandbox create --template pi`
needs only providers and a policy. Templates live in the gateway; recreate
them after a fresh VM.

### Developing plugins without rebuilding

While you write an extension or skill, mount its directory instead of baking
it. `brig sandbox create` gives the running VM a read-only sandbox mount of the
directory, which brig exposes as a Podman volume, and creates a sandbox with
that volume attached; what follows `--` goes to `openshell sandbox create`:

```sh
brig sandbox create dev --mount ~/src/pi-kit:/usr/local/share/pi-kit-dev -- \
  --name kit-dev --from localhost/pi-agent:2026-10-08 --provider llm
# in the sandbox
pi-sandbox -e /usr/local/share/pi-kit-dev
```

The VM keeps the mount for later sandboxes; `brig show dev` lists it with its
volume, and `brig update dev --remove-mount` removes it once no sandbox uses it.

Edits on the host appear in the sandbox at once; `/reload` in Pi picks them
up. The mount is read-only, so the agent cannot change your plugin source, and
mounting under `/usr` keeps it readable under the default policy. When the
plugin is done, copy it into `pi-kit/` and rebuild.

Repository-level `.pi/extensions`, `.pi/skills` and `.pi/prompts` also work,
after Pi asks you to trust the project. Package declarations in a repository's
`.pi/settings.json` do not, because Pi would have to fetch them from npm.

## Claude Code with a subscription

[Claude Code](https://code.claude.com) can run on your Claude Pro or Max
subscription. `claude setup-token` makes a token for it that is valid for a
year; the sandbox gets a placeholder, and the proxy adds the real token on
requests to `api.anthropic.com`, which the profile opens to Claude Code only.
As with Pi, the image holds the program, your instructions and your skills, and
`/sandbox` holds what Claude Code writes.

| What | Where | Why |
| --- | --- | --- |
| Claude Code, git, gh, ripgrep | image, `/usr/local/bin` and `/usr/bin` | pinned at build time; Claude Code's own updates are off |
| No updates, telemetry or error reports | image, `/etc/claude-code/managed-settings.json` | managed settings win over user and project settings |
| Your standing instructions and skills | image, `/usr/local/etc/claude` and `/usr/local/share/claude-kit/skills` | linked into the config dir, so a new image brings new versions |
| Claude Code's config dir: `.claude.json`, `settings.json`, sessions under `projects/` | `/sandbox/.claude` via `CLAUDE_CONFIG_DIR` | writable and persistent |

### The image

```text
claude-image/
  Containerfile
  claude-sandbox              # start script, below
  managed-settings.json
  etc/CLAUDE.md               # your standing instructions
  skills/                     # your skills, one directory each
```

<!-- agent-file: claude agents/claude/Containerfile -->
```dockerfile
# Containerfile
FROM registry.fedoraproject.org/fedora-minimal:44

ARG CLAUDE_VERSION=2.1.295
RUN dnf -y install --setopt=install_weak_deps=False gh git-core jq ripgrep tar \
 && dnf clean all
# The native installer installs into $HOME; keep only its single binary.
RUN curl -fsSL https://claude.ai/install.sh | HOME=/tmp/claude bash -s "$CLAUDE_VERSION" \
 && install -m 755 "$(readlink -f /tmp/claude/.local/bin/claude)" /usr/local/bin/claude \
 && rm -rf /tmp/claude

COPY --chmod=644 managed-settings.json /etc/claude-code/managed-settings.json
COPY etc/ /usr/local/etc/claude/
COPY skills/ /usr/local/share/claude-kit/skills/
COPY --chmod=755 claude-sandbox /usr/local/bin/claude-sandbox

RUN git config --system credential.https://github.com.helper \
      '!/usr/bin/gh auth git-credential' \
 && useradd --create-home --uid 1000 claude

USER claude
WORKDIR /sandbox
ENV CLAUDE_CONFIG_DIR=/sandbox/.claude \
    GH_CONFIG_DIR=/sandbox/.config/gh \
    GIT_CONFIG_GLOBAL=/sandbox/.gitconfig
```

The managed settings turn off what would only show up as denials: updates,
telemetry, error reports (on by default for subscriptions) and Claude Code's
bundled ripgrep, in favour of the image's. The file must be readable by every
user, or Claude Code silently ignores it:

<!-- agent-file: claude agents/claude/managed-settings.json -->
```json
{
  "env": {
    "DISABLE_UPDATES": "1",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "DISABLE_ERROR_REPORTING": "1",
    "USE_BUILTIN_RIPGREP": "0"
  }
}
```

The start script links what the image keeps managing into the config dir:

<!-- agent-file: claude agents/claude/claude-sandbox -->
```sh
#!/bin/sh
# claude-sandbox: start Claude Code with this image's instructions and skills
set -e
mkdir -p "$CLAUDE_CONFIG_DIR"
ln -sf /usr/local/etc/claude/CLAUDE.md "$CLAUDE_CONFIG_DIR/CLAUDE.md"
ln -sfn /usr/local/share/claude-kit/skills "$CLAUDE_CONFIG_DIR/skills"
exec claude "$@"
```

```sh
podman build -t localhost/claude-agent:2026-10-09 claude-image/
brig image push dev localhost/claude-agent:2026-10-09
```

### Subscription token

On the host, `claude setup-token` signs you in through the browser and prints
the token without saving it. Store it in the keyring:

```sh
claude setup-token
secret-tool store --label='Claude Code token (agents)' application brig service claude.ai user alice
```

With the provider file below in place, `claude setup-token | brig secret set
dev claude-code` stores and syncs it in one step.

Start from OpenShell's
[`providers/claude-code.yaml`](https://github.com/NVIDIA/OpenShell/blob/v0.1.4/providers/claude-code.yaml)
and replace its API key with the subscription token, as below. Keep
`api.anthropic.com`, and add `platform.claude.com` read-only: Claude Code's
first-run setup checks it and exits when it cannot reach it. With the image's
settings, Claude Code needs nothing else.

<!-- agent-file: claude profiles/claude-code.yaml -->
```yaml
# profiles/claude-code.yaml: after OpenShell's, with other credentials,
# endpoints and binaries
id: claude-code
display_name: Claude Code
description: Claude Code on a Claude subscription token from claude setup-token
category: agent
credentials:
  - name: oauth_token
    description: Claude subscription token from claude setup-token
    env_vars: [CLAUDE_CODE_OAUTH_TOKEN]
    required: true
    auth_style: bearer
    header_name: authorization
discovery:
  credentials: [oauth_token]
endpoints:
  - host: api.anthropic.com
    port: 443
    protocol: rest
    access: read-write
    enforcement: enforce
  - host: platform.claude.com
    port: 443
    protocol: rest
    access: read-only
    enforcement: enforce
binaries: [/usr/local/bin/claude]
```

```yaml
# ~/.config/brig/openshell/providers/claude-code.yaml
name: claude-code
type: claude-code
credentials:
  CLAUDE_CODE_OAUTH_TOKEN:
    secret_tool:
      lookup: [service, claude.ai, user, alice]
```

Claude Code sends the placeholder in `CLAUDE_CODE_OAUTH_TOKEN` as a bearer
token, and the proxy swaps in your token. Three things to know:

- An `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` in the sandbox wins over
  the subscription, so do not attach a provider that sets one, such as
  OpenShell's unchanged `claude-code` or `anthropic` profile.
- The token only makes model requests: claude.ai connectors and Remote Control
  do not work with it. Do not run `/login` in a sandbox: it would store a real
  token in `/sandbox/.claude`.
- After a year, run `claude setup-token` again, store the new token under the
  same attributes (`brig secret set dev claude-code`). Then start Claude Code
  from a new shell, `brig sandbox exec dev webapp-claude --tty -- bash -l`, or
  stop and start the sandbox: a Claude Code restarted in an old shell keeps
  sending the old token.

### Start it

```sh
brig sync dev                     # applies the profile and provider above
brig use dev
openshell sandbox create --name webapp-claude \
  --from localhost/claude-agent:2026-10-09 \
  --provider claude-code --provider github
# now in a login shell inside the sandbox
git clone https://github.com/alice/webapp.git && cd webapp
claude-sandbox
```

The first start walks through Claude Code's setup screens once; the answers
stay in `/sandbox/.claude`. `claude-sandbox --continue` resumes the last
conversation in the current directory. Detaching, templates and [Git
access](git-access.md) work as for Pi.

## opencode

[opencode](https://opencode.ai) is an open-source coding agent that talks to
most model providers. It cannot use a Claude subscription: Anthropic limits
subscription tokens to Claude Code and claude.ai, and opencode dropped its
subscription login. Point it at an OpenAI-compatible endpoint, as for Pi, or
at Anthropic's API with an API key. The image follows the pattern of the two
above: the program and its settings in the image, what opencode writes in
`/sandbox`.

| What | Where | Why |
| --- | --- | --- |
| opencode, git, gh, ripgrep | image, `/usr/local/bin` and `/usr/bin` | pinned at build time; opencode's own updates are off |
| Updates, sharing and downloads off | image, `/etc/opencode/opencode.json` and the `OPENCODE_DISABLE_*` variables | opencode's managed config on Linux, which user and project config cannot override |
| Your model endpoint and its models | the same managed file | a new image brings a new model list |
| Your standing instructions | image, `/usr/local/etc/opencode/AGENTS.md` | linked into the config dir, so a new image brings new versions |
| opencode's sessions and `auth.json` | `/sandbox/.local/share/opencode` via `XDG_DATA_HOME`, with the cache and state dirs under `/sandbox` too | writable and persistent |

### The image

```text
opencode-image/
  Containerfile
  opencode-sandbox            # start script, below
  etc/opencode.json           # managed config: endpoint, models, switches
  etc/AGENTS.md               # your standing instructions
```

The npm package `opencode-ai` starts a platform binary that it keeps under
`node_modules`. A sandbox policy admits programs by their real path, so the
Containerfile installs the package into a scratch prefix, copies the one
binary to `/usr/local/bin/opencode` and throws the rest away. The build fails
if the package has no binary for the host's architecture.

<!-- agent-file: opencode agents/opencode/Containerfile -->
```dockerfile
# Containerfile
FROM registry.fedoraproject.org/fedora-minimal:44

ARG OPENCODE_VERSION=1.19.0
RUN dnf -y install --setopt=install_weak_deps=False \
      findutils gh git-core jq nodejs24 nodejs24-bin nodejs24-npm-bin ripgrep tar \
 && dnf clean all \
 && useradd --create-home --uid 1000 opencode
# opencode's npm package starts a platform binary from node_modules. Keep only
# that binary, at the path the profile admits, and no node_modules.
RUN set -e; \
    npm install -g --prefix /tmp/oc --ignore-scripts "opencode-ai@${OPENCODE_VERSION}"; \
    arch="$(uname -m | sed -e 's/x86_64/x64/' -e 's/aarch64/arm64/')"; \
    bin="$(find /tmp/oc -type f -path "*/opencode-linux-${arch}/bin/opencode" | head -n 1)"; \
    [ -n "$bin" ] || { echo "opencode-ai ${OPENCODE_VERSION} has no opencode-linux-${arch} binary" >&2; exit 1; }; \
    install -m 755 "$bin" /usr/local/bin/opencode; \
    rm -rf /tmp/oc /root/.npm

COPY --chmod=644 etc/opencode.json /etc/opencode/opencode.json
COPY etc/AGENTS.md /usr/local/etc/opencode/AGENTS.md
COPY --chmod=755 opencode-sandbox /usr/local/bin/opencode-sandbox

RUN git config --system credential.https://github.com.helper \
      '!/usr/bin/gh auth git-credential'

USER opencode
WORKDIR /sandbox
ENV XDG_CONFIG_HOME=/sandbox/.config \
    XDG_DATA_HOME=/sandbox/.local/share \
    XDG_CACHE_HOME=/sandbox/.cache \
    XDG_STATE_HOME=/sandbox/.local/state \
    GH_CONFIG_DIR=/sandbox/.config/gh \
    GIT_CONFIG_GLOBAL=/sandbox/.gitconfig \
    OPENCODE_DISABLE_AUTOUPDATE=1 OPENCODE_DISABLE_LSP_DOWNLOAD=1 \
    OPENCODE_DISABLE_MODELS_FETCH=1
```

The managed config turns off updates and sharing, and names the model
endpoint. `{env:LLM_API_KEY}` is filled in from the sandbox's environment,
where the provider puts a placeholder; the proxy swaps in your key on requests
to the profile's host. It is the Pi pair of `profiles/llm.yaml` and provider
described [above](#model-endpoint), and Pi and opencode can share both:

<!-- agent-file: opencode agents/opencode/etc/opencode.json -->
```json
{
  "$schema": "https://opencode.ai/config.json",
  "autoupdate": false,
  "share": "disabled",
  "model": "llm/your-model-id",
  "provider": {
    "llm": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "LLM endpoint",
      "options": {
        "baseURL": "https://llm.example.com/v1",
        "apiKey": "{env:LLM_API_KEY}"
      },
      "models": {
        "your-model-id": { "name": "your-model-id" }
      }
    }
  }
}
```

For Anthropic's API, `brig agent init opencode --anthropic` writes a config
without the `provider` block, since opencode knows Anthropic, and this profile
in place of `llm.yaml`. Keep the API key in the keyring like any other secret
(`brig secret set dev anthropic`):

```yaml
# profiles/anthropic.yaml: Anthropic's API with an API key, for opencode
id: anthropic
display_name: Anthropic API
description: Anthropic's API with an API key; a subscription token only works in Claude Code
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
binaries: [/usr/local/bin/opencode]
```

The start script links the instructions into opencode's config dir, which is
under `/sandbox`:

<!-- agent-file: opencode agents/opencode/opencode-sandbox -->
```sh
#!/bin/sh
# opencode-sandbox: start opencode with this image's instructions
set -e
mkdir -p "$XDG_CONFIG_HOME/opencode" "$XDG_DATA_HOME/opencode"
ln -sf /usr/local/etc/opencode/AGENTS.md "$XDG_CONFIG_HOME/opencode/AGENTS.md"
exec opencode "$@"
```

Build, push, start and detach as for the other two:

```sh
brig agent build dev opencode
brig sandbox create dev --connect -- --name webapp-oc --template opencode \
  --provider llm --provider github
# in the sandbox
git clone https://github.com/alice/webapp.git && cd webapp
opencode-sandbox
```

Do not run `opencode auth login` in a sandbox: it would store a real key in
`auth.json`. opencode talks to an OpenAI-compatible endpoint through the npm
package `@ai-sdk/openai-compatible`. If a version fetches it on first use
instead of bundling it, the sandbox denies the request, and the Containerfile
has to install the package at build time.
