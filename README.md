# brig

`brig` manages the lifecycle of agent-sandbox VMs on a Fedora laptop. Each VM
runs headless Fedora with an [NVIDIA
OpenShell](https://github.com/NVIDIA/OpenShell) gateway, so autonomous agents
are contained by a VM boundary in addition to OpenShell's own sandboxing. Your
host's `openshell` CLI drives each VM's gateway; brig registers it as
`brig-<vm>`. [docs/openshell.md](docs/openshell.md) shows how to run agents on
it, from a first sandbox to Git access and daily use.

> **Status:** early development. Expect breaking changes.
>
> brig currently supports and is tested on x86_64 Fedora hosts.

## Install

On Fedora, the recommended way is brig's [package
repository](#package-repository), which also has the `openshell` CLI that must
match the VMs' OpenShell version:

```sh
sudo rpm --import https://dennisklein.github.io/brig/RPM-GPG-KEY-brig
sudo dnf install https://dennisklein.github.io/brig/brig-release.noarch.rpm
sudo dnf install brig
brig doctor
```

The `brig` package requires everything brig needs on the host, including the
`openshell` CLI and, with SELinux policy, the `brig-selinux` package (see
[Troubleshooting](#troubleshooting)). It recommends virtiofsd for `--mount`,
Podman for `brig image push` and libsecret's secret-tool for providers in
[OpenShell config directories](#openshell-config-directories); `dnf install
--setopt=install_weak_deps=False brig` skips them.

Or install the binary with [mise](https://mise.jdx.dev):

```sh
mise use -g github:dennisklein/brig
```

Or with Go 1.26 or newer:

```sh
go install github.com/dennisklein/brig@latest
```

With mise or Go, install what brig needs on the host, including the
`openshell` CLI from the package repository, and check the result:

```sh
sudo rpm --import https://dennisklein.github.io/brig/RPM-GPG-KEY-brig
sudo dnf install https://dennisklein.github.io/brig/brig-release.noarch.rpm
sudo dnf install $(brig print-fedora-deps)
sudo dnf install --setopt=install_weak_deps=False openshell
brig doctor
```

Add `--with mounts` to `print-fedora-deps` for `--mount` (virtiofsd), `--with
push` for `brig image push` (Podman), `--with secrets` for providers in
[OpenShell config directories](#openshell-config-directories) (secret-tool),
or `--with all` for all of them.

## Quick start

```sh
brig create dev                 # builds a base image first if needed
brig list
brig use dev                    # point this shell's openshell CLI at the VM's gateway
openshell sandbox create        # a shell in OpenShell's default image
brig ssh dev                    # a shell in the VM, as the gateway's user
```

`brig use` needs brig's [shell integration](#shell-integration), which the
package sets up for new bash, zsh and fish sessions: open a new terminal after
installing it. Without the package, add the integration to your shell's
startup file, or run `eval "$(brig env dev)"` instead.

When you are done, shut the VM down, or delete it with everything on it:

```sh
brig stop dev
brig delete dev                 # asks first; the VM's data disk goes with it
```

To run an agent, build an image for it and give it credentials as described in
[the guide](docs/openshell.md#your-first-agent-sandbox-pi).

Share host directories with `--mount SOURCE[:TARGET][:OPTIONS]`, where OPTIONS
is a comma-separated list of `ro` (the default), `rw` and `sandbox`. In the
VM, user `agent` owns what you own there, and what it writes is yours; the
VM's other users map to your subordinate IDs, which need entries in
`/etc/subuid` and `/etc/subgid` (Fedora adds them for new users). With
`sandbox`, OpenShell sandboxes can attach the directory read-only as a Podman
volume named after the mount's tag, which `brig show` lists (e.g. `brig0`).
`brig update --add-mount` and `--remove-mount` change the mounts of a running
VM at once.

`brig sandbox create` does all of this in one step: it gives the VM a sandbox
mount of each directory it lacks one of and runs `openshell sandbox create`
on the VM's gateway with the volumes attached, passing on what follows `--`:

```sh
brig sandbox create dev --mount ~/src/pi-kit:/usr/local/share/pi-kit -- --name kit
```

The guide has an
[example](docs/openshell.md#developing-plugins-without-rebuilding).

## Shell integration

The `openshell` CLI talks to the gateway that `OPENSHELL_GATEWAY` names, so
each shell picks its VM. The brig package loads brig's shell integration in
new interactive bash and zsh sessions (`/etc/profile.d/brig.sh`) and fish
sessions. Otherwise load it from your shell's startup file to switch with
`brig use NAME`:

```sh
eval "$(brig shell-init bash)"    # ~/.bashrc
eval "$(brig shell-init zsh)"     # ~/.zshrc
brig shell-init fish | source     # ~/.config/fish/config.fish
```

It defines a `brig` shell function that runs `brig use NAME` and applies what
it prints, as `eval "$(brig env NAME)"` would; `brig use --unset` points the
CLI at no VM. Every other command runs brig as before.

A gateway has workspaces, and `-w WORKSPACE` picks one on every command that
works in one: `sync` (repeatable, default every workspace), `sandbox list`
(default every workspace), the other `sandbox` commands, `policy diff` and
`policy save` (default `default`). `brig use dev -w team`
also exports `BRIG_WORKSPACE=team`, which is the default of `-w` in that
shell, and `OPENSHELL_WORKSPACE=team`, which makes plain `openshell` commands
work in the same workspace; `brig use dev` clears it, and `brig use -w team` changes the workspace
and keeps the VM.

In zsh it also defines a [Powerlevel10k](https://github.com/romkatv/powerlevel10k)
segment named `brig`, which shows the VM the shell points at, such as `▥
dev`. Add `brig` to `POWERLEVEL9K_LEFT_PROMPT_ELEMENTS` or
`POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS` in `~/.p10k.zsh`, and style it like other
segments, for example:

```zsh
typeset -g POWERLEVEL9K_BRIG_FOREGROUND=208                   # the default
typeset -g POWERLEVEL9K_BRIG_VISUAL_IDENTIFIER_EXPANSION='VM'  # instead of ▥
```

Other prompts can show `${OPENSHELL_GATEWAY#brig-}` when `OPENSHELL_GATEWAY`
starts with `brig-`.

## How it works

- **VMs** run rootless in your libvirt user session (`qemu:///session`) with
  UEFI, KVM and passt user-mode networking. They show up in virt-manager as
  `brig-<vm>`, but start them with `brig start`, which also starts their
  network.
- **Base images** are built on your laptop with
  [mkosi](https://github.com/systemd/mkosi), without root: minimal Fedora plus
  OpenShell from brig's package repository.
- **Disks:** each VM boots from a disposable overlay of a read-only base
  image. Everything that must survive, including OpenShell's state,
  credentials and container storage, lives on a separate data disk mounted at
  `/home/agent`. `brig upgrade` moves a VM onto a newer base image and keeps
  the data disk.
- **Configuration** of each boot (SSH keys, hostname, mounts) is passed as
  systemd credentials. There is no cloud-init.
- **Network profiles** restrict what a VM may reach: the internet, the LAN,
  services on the host. They are enforced outside the VM, so even root in the
  VM cannot lift them: each VM's passt runs in a private network namespace,
  connected to the host by pasta, whose nftables rules brig sets up without
  root privileges. This adds to OpenShell's own per-sandbox egress policy.
- **Provider secrets** never reach the host's disk or a command line. Your
  host's `openshell` CLI sends them straight to the VM's gateway, either when
  you create a provider yourself or when `brig sync` reads them from your
  keyring and hands them to the CLI in its environment. The gateway stores
  them encrypted on the VM's data disk, next to the key that decrypts them,
  so treat a VM's directory as holding its secrets. brig also copies the gateway's
  mTLS client certificate and key into the CLI's configuration.
- **Audit trail:** while a VM runs, a systemd user unit on the host,
  `brig-audit-<vm>.service`, records what its sandboxes did into
  `$XDG_STATE_HOME/brig/audit/<vm>/`, outside the VM: every request that
  OpenShell's proxy allowed or denied, sandboxes created and deleted, each
  revision of a sandbox's policy with the rules it changed and who made it,
  the rules OpenShell drafted and the decisions on them, the brig commands
  that worked on the VM, and gaps where records may be missing. The files are
  JSON lines, one per day, compressed once the day is over; they stay after
  `brig delete`. Changes, but not allowed requests, also go to the user
  journal (`journalctl --user -u brig-audit-<vm>`).

## Commands

| Command | Purpose |
|---|---|
| `create NAME` | create and start a VM; stops first if a [`brig doctor`](#install) check fails |
| `up [NAME]` | start the VM, or create it with `--create` (and `brig create` flags after `--`), wait until its gateway answers and its config directories sync, then list its sandboxes and the drafted rules that wait for review (`-o json`); safe to repeat |
| `list`, `show [NAME]` | list VMs (queried in parallel), show one (`-o json`, which adds cumulative CPU time and memory use of running VMs as `stats`) |
| `start [NAME]`, `stop [NAME]` | boot and re-register the gateway, shut down; `start --wait` (also on `create`) fails unless the gateway answers and the config directories sync |
| `update [NAME]` | change CPUs, memory, disks and network profile (at the next boot), mounts (at once) and OpenShell config directories |
| `sync [NAME]` (`-w`) | apply the VM's OpenShell config directories to its gateway |
| `agent init claude\|pi\|opencode [NAME]` | write an image definition, provider profile, provider and default policy for a coding agent into config directories, never overwriting; with `--vm`, also attach them, store missing secrets, sync and build the image |
| `agent build [NAME] AGENT` (`-w`) | build the agent's image with Podman, copy it into the VM and point the agent's gateway template at it |
| `secret set [NAME] PROVIDER [CREDENTIAL]` (`-w`) | store a provider's secret in the keyring under the attributes its provider file gives (read without echo, or from standard input), then sync just that provider if the VM runs (`--no-sync`) |
| `secret list [NAME]` (`-w`) | show whether each credential of the VM's providers is stored or missing, never the value (`-o json`) |
| `upgrade [NAME]` | move onto the newest base image, keeping the data disk and rolling back on failure |
| `delete [NAME]` | delete the VM, its disks and its gateway registration; asks first on a terminal (`--yes` skips the question, `--force` deletes a running VM) |
| `ssh [NAME]`, `console [NAME]` | shell (`--root` for root), serial console |
| `use [NAME]` | point the shell's `openshell` CLI at the VM's gateway and default sandbox policy; `--unset` at none (needs the [shell integration](#shell-integration)); without NAME, say which VM the shell points at; `-w WORKSPACE` also exports `BRIG_WORKSPACE` |
| `sandbox create [NAME]` | run `openshell sandbox create` on the VM's gateway, with host directories mounted read-only (`--mount DIR:TARGET`) and only the providers that `--provider` names, as `brig ui` does, in the workspace of `-w`; `--connect` starts the VM if needed, waits until the sandbox is ready and connects to it |
| `sandbox list [NAME]` | list the sandboxes in every workspace of the VM's gateway, or of every running VM with `--all-vms` (`-o json`) |
| `sandbox connect/exec/logs/start/stop/delete [NAME] SANDBOX` | run the `openshell` command on the VM's gateway, in the workspace of `-w`, once the sandbox is found; `delete` asks first on a terminal (`--yes` skips the question), `exec` exits with the command's status |
| `sandbox attach/detach [NAME] SANDBOX PROVIDER` | attach or detach a provider and wait until the sandbox applies it |
| `policy diff [NAME] SANDBOX` (`-w`) | compare a sandbox's network rules with the policy file it came from; exit status 1 if they differ (`-o json`) |
| `policy allow [NAME] SANDBOX HOST[:PORT]` (`-w`) | add one rule that lets `--binary PATH` reach HOST, as `openshell policy update` does but with brig's annotations on the revision (`--access`, `--l4`, `--audit`); refuses `*`, `**` and `*.com`, and wants `--broad` for full access or `--l4`; `--draft ID` approves a draft; `--save` also writes the rule into the policy file |
| `policy save [NAME] SANDBOX` (`-w`) | write the rules a sandbox has and its policy file lacks into that file, keeping its comments (`--rule KEY`, `--dry-run`); on a terminal it shows the change and the hosts it allows and asks first (`-y` skips); a sandbox from the default policy needs `--to FILE` |
| `audit [NAME] [SANDBOX]` | show the [audit trail](#how-it-works) as a timeline, by default of the last day (`--since`, `--until`, `--denied`, `--host`, `--kind`, `-f` to follow, `-o json`); `--summary` lists each endpoint a sandbox reached |
| `audit status [NAME]`, `audit prune [NAME]` | show whether each VM's collector runs and the space its history takes; delete history from before a date or age (`--before 90d`), of one sandbox (`--sandbox`) or of deleted VMs (`--deleted-vms`) |
| `env NAME` | print `export OPENSHELL_GATEWAY=brig-NAME` and the path of the default sandbox policy as `OPENSHELL_SANDBOX_POLICY` (fish syntax when `$SHELL` is fish); use as `eval "$(brig env NAME)"` |
| `image build/list/rm/prune` | manage base images |
| `image push [NAME] IMAGE` | copy a container image from the host's Podman into a VM |
| `doctor`, `print-fedora-deps` | check host prerequisites, list the Fedora packages they need |
| `ui` | open the [web UI](#web-ui) in your browser |
| `shell-init SHELL` | [shell integration](#shell-integration) for bash, zsh and fish |
| `completion SHELL` | shell completion for bash, zsh, fish and PowerShell |
| `help topics`, `help TOPIC` | list the concepts and how-tos brig explains, explain one, such as `brig help fence-2` or `brig help unblock` |

Where a command takes `[NAME]`, leaving it out means the VM that `brig use`
selected; the command says which on standard error. `brig COMMAND --help`
lists each command's flags, and `brig help topics` the concepts, such as
fences, gateways and providers, and the how-tos that `brig help TOPIC`
explains.

## Progress output

Commands that take a while, such as `create`, `start`, `stop`, `upgrade`,
`delete` and `image build`, show what they are doing on standard error, so
standard output stays clean for scripts. Steps that the command goes on
without, such as a gateway that does not answer at `brig start`, end as
skipped with the reason. `ssh`, `console`, `env`, `use`, `shell-init` and
`sandbox create` show no progress, since they own the terminal or must print
exactly their output, and neither does `ui`.

`--progress MODE` (or `BRIG_PROGRESS`) picks the display: `auto` (the
default) chooses from where standard error goes, `tty` redraws a tree of
steps, `counter` a single line, `plain` prints one line per event, and `none`
shows nothing. These commands also append a JSON event log of their work to
`$XDG_STATE_HOME/brig/activity.jsonl` (usually
`~/.local/state/brig/activity.jsonl`), which brig moves to
`activity.jsonl.1` once it passes 8 MiB. `--progress-log FILE` (or
`BRIG_PROGRESS_LOG`) logs to FILE instead; an empty value turns the log off.
A caller can pass a W3C trace context in `TRACEPARENT` and `TRACESTATE` to
find a run in the log by its trace id.
On a terminal, `NO_COLOR` turns colours off. Text from guests, images and
libvirt is escaped before it is shown, so it cannot inject terminal control
sequences.

## Scripting

brig's exit status says why a command failed:

| Status | Meaning |
|---|---|
| 0 | success |
| 1 | any other failure, including wrong arguments |
| 3 | not found: no such VM, image, sandbox or workspace |
| 4 | exists already, such as the VM that `brig create` would create |
| 5 | not ready: the VM does not run or boot, its gateway does not answer, or the sandbox is not ready |
| 6 | applying the VM's OpenShell config directories failed (`brig sync`, `--wait`) |

`brig policy diff` uses 1 and 2 for its outcomes (see its `--help`).

Standard output carries only a command's result: a table, JSON, or one
line such as `Created VM dev from image …`. Notes, hints and warnings go to
standard error, as progress does.

`-o json` works on the commands that read and on those that change a VM:
`create`, `start`, `stop`, `update` and `upgrade` print the VM as `show -o
json` does once they are done, `sync -o json` prints each workspace, profile
and provider with what it did to it (or would, with `--dry-run`) and the
error if the sync failed, and `image push -o json` prints the VM and the
image.
With `-o json`, a command that fails before it printed anything prints the
error on standard output as well, as `{"error": "...", "reason": "not_ready",
"exit_code": 5}`; the reasons are `failed`, `not_found`, `exists`,
`not_ready` and `sync_failed`.

## Web UI

`brig ui` serves a web UI on 127.0.0.1 and opens it in your browser: your
VMs with their state, CPU and memory use, mounts, network profile, OpenShell
config directories and last sync; what needs your attention; the `brig
doctor` checks; and what brig did, from the [activity log](#progress-output).
From each running VM's OpenShell gateway, which it reads over gRPC with the
client certificate `brig start` keeps, it shows the sandboxes with their
providers and base and effective policies, and the gateway's providers,
provider profiles, default policy and templates; credentials only by name.
Pages update live.

Traffic shows each sandbox's requests as OpenShell's proxy decided them,
from the gateway's log of the last hour, with the rule that allowed a
request or the reason it was denied; a line brig ui cannot read shows as it
came, and URL queries are cut, since they may hold tokens. Denials of the
last hour show on the Overview, under Needs you and on the sandbox's Traffic
tab. Topology draws the path from this host through each gateway and sandbox
to the providers' endpoints and the hosts sandboxes reached or were denied.
Drops by a VM's network profile are not logged yet.

The UI also changes things: start, stop, create, delete and upgrade a VM,
change its mounts, config directories, profile, CPUs, memory and disks,
preview and run `brig sync`, and build, prune and push base images. For
sandboxes it creates them from a template or an image with providers, a
policy file and host directories (`brig sandbox create`), starts, stops and
deletes them, and attaches and detaches providers (`openshell`, always with
`-g brig-NAME`). Connect shows the commands to run in your own terminal;
there is no terminal in the browser. A sandbox's Files tab lists `/sandbox`
and its directories by running one read-only `find` in the sandbox through its
gateway, and shows the command that does the same. Every
change is a dialog that shows the `brig` or `openshell` command it runs, with
a Copy button, and runs exactly that command; the browser never sends a
command line. Deleting a VM or a sandbox asks you to type its name. A running command is a card
with its steps and, if it failed, its output. A VM's buttons wait while a
command works on it. Ctrl-C in the terminal that runs `brig ui` interrupts
the running commands the way it does in a shell, so brig rolls back. The UI
shows and takes no secrets.

**Allow…** on a denied request offers the rule that OpenShell drafted from
it, which names the program's full path (`brig policy allow --draft`, which
runs `openshell rule approve`), or a rule of your own with its access level
and enforce or audit (`brig policy allow`, whose revision records that brig
made it). Either applies to that sandbox at once. Tick **Keep for
new sandboxes** to write the rule into the sandbox's policy file in the same
step (`brig policy allow --save`); otherwise **Save to file…** previews and
runs `brig policy save` next. Allow refuses `*`, `**` and wildcards such as
`*.com`, and asks for a tick before it adds a rule with full access, for the
whole connection or for a program path with a wildcard. A request denied
because a provider's credential may not go to its host is allowed only after
you confirm that it goes out without it. **Reject…** drops a draft that you
do not want (`openshell rule reject`).

Traffic also reads the [audit trail](#how-it-works): pick 24 hours, 7 days
or all instead of the last hour, and a sandbox that was deleted, whose
history stays. A sandbox's History tab lists its policy revisions with who
made each, its drafts and lifecycle, and the endpoints it reached in the
last week. Host shows each VM's collector, its gaps and the space its
history takes by sandbox, and **Prune…** runs `brig audit prune`. A running
VM whose traffic nothing records shows under Needs you.

Only you can open it. It prints a URL with a token that is new each run and
swaps it for a cookie, answers only to `127.0.0.1` and `localhost`, refuses
connections from other users' processes, and loads nothing from the
network. A second `brig ui` opens the running one. `--port` picks the port,
`--no-open` only prints the URL, and `--demo` serves made-up VMs without
touching the host.


`$XDG_CONFIG_HOME/brig/config.yaml` (usually `~/.config/brig/config.yaml`)
overrides the built-in defaults shown here and can add network profiles, such
as `ollama` below. A profile in the file replaces a built-in profile of the
same name as a whole.

```yaml
defaults:
  cpus: 4
  memory: 8GiB
  root_disk: 20GiB
  data_disk: 40GiB
  network_profile: default
  fedora_release: 44

network_profiles:
  default:              # internet only
    internet: true
  open:                 # everything, including the host and the LAN
    internet: true
    host: true
    lan: true
    ipv6: true
  isolated: {}          # nothing; push images with brig image push
  ollama:               # not built in: internet plus a model server on the host
    internet: true
    host_ports: [11434]

openshell:
  secret_tool: secret-tool   # looks up provider secrets; a name or an absolute path
  configs: []                # OpenShell config directories of new VMs, e.g. [~/src/agent-openshell]

audit:
  journal: changes           # mirror to the user journal: changes, all (requests too) or off
  retention: 0               # delete days of the audit trail older than this, e.g. 90d or 12w; 0 keeps all
```

The host is reachable from a VM at the VM's default gateway address; with
`host_ports`, only those TCP ports are. Without `host`, the host's own
addresses are blocked too. The LAN means the private, shared (100.64.0.0/10),
link-local, multicast and broadcast address ranges plus the networks of the
host's addresses and of its routes other than default ones, such as a VPN's
intranet. brig reads them when a VM starts: restart VMs after joining another
network or connecting a VPN.

A profile that allows the internet, the LAN or the host also gets DNS from the
host's resolver, for any name; other profiles, such as `isolated` or one with
only `host_ports`, get none.

A VM's gateway pulls OpenShell's container images from ghcr.io and nvcr.io and
refreshes provider tokens itself, so profiles without `internet` need images
pushed into the VM.

## OpenShell config directories

An OpenShell config directory holds what you would otherwise set up in each
VM's gateway by hand: provider profiles (`profiles/*.yaml`), providers whose
secrets brig looks up in your keyring with `secret-tool`, or reads from a
`BRIG_SECRET_*` environment variable in CI (`providers/*.yaml`, which cannot
hold values), and a default sandbox policy
(`policies/default.yaml`). brig applies a VM's directories to its gateway at
every `brig start` and on `brig sync NAME`.

Attach directories with `brig create --openshell-config DIR` or `brig update
--add-openshell-config DIR`, or list defaults for new VMs in
`openshell.configs`. A provider's secrets may be sent to every endpoint of its
profile, so check a shared profile before you pair it with your own secrets;
`brig sync --dry-run` names the endpoints. Without `--prune`, brig never
deletes anything. `brig secret set VM PROVIDER` stores a provider's secret in
the keyring under the attributes its file gives, and `brig secret list` shows
which are stored. [The guide](docs/openshell.md#openshell-config-directories)
has a worked example, and `brig sync --help` the rules.

## Package repository

brig's VM images, and your host, get OpenShell from
<https://dennisklein.github.io/brig/>, a signed dnf repository that the
[packages workflow](.github/workflows/packages.yaml) fills: it builds each new
OpenShell release and each new brig release from source for the Fedora
releases listed in [`packaging/fedora-releases`](packaging/fedora-releases).
It checks daily and after each brig release. See [`packaging/`](packaging/)
and [package updates](#package-updates).

## Troubleshooting

**`brig start` or the *VM network* check of `brig doctor` fails, and passt's
log says `Couldn't write to /proc/N/uid_map: Operation not permitted`.** brig runs passt as root in pasta's user
namespace, so passt needs the `setfcap` capability there to sandbox itself,
which the SELinux policy of passt 0^20261002 does not grant it. `sudo ausearch
-m AVC -c passt` shows the denial (`{ setfcap } … tclass=cap_userns`). Until
the policy allows it, the `brig-selinux` package loads a module that does;
`dnf install brig` installs it with SELinux policy. If you installed brig with
mise or `go install`, add the module yourself:

```sh
echo '(allow passt_t self (cap_userns (setfcap)))' > /tmp/brig-passt.cil
sudo semodule -i /tmp/brig-passt.cil
```

`sudo semodule -r brig-passt` removes it again, which you want once
`brig-selinux` is installed. The guide covers
[problems inside the VM](docs/openshell.md#troubleshooting).

## Development

Tool versions are pinned in `mise.toml`:

```sh
mise install
go test ./...
golangci-lint run ./...
goreleaser release --snapshot --clean   # local release dry run
```

`go run . ui --demo --dev-assets internal/ui/web` serves the web UI's
templates and static files from disk, read again on each request.

### Releasing

Releases are cut from annotated tags; the tag message becomes the release
notes:

```sh
git tag -a v0.1.0      # write the release notes in the editor
git push origin v0.1.0
```

Once the release is published, the workflow starts the [packages
workflow](#package-updates), which builds the `brig` and `brig-selinux`
packages for it. Pre-releases (`v0.2.0-rc.1`) are not packaged.

### Package updates

Two workflows keep the [package repository](#package-repository) current:

- [`packages`](.github/workflows/packages.yaml) runs daily, on pushes to
  `main` that change `packaging/`, and on demand. If upstream's latest
  OpenShell release is not published yet, it builds `openshell`,
  `openshell-gateway`, `openshell-prover` and `python3-openshell` from the
  release's source, signs them, smoke-tests the repository and only then
  publishes it, keeping the previous version and the newest one of the previous
  minor line, so `--openshell` can still pin it. It skips a release that the
  repository would not keep, such as an older backport, unless rebuild is
  requested. A push republishes the site even when nothing needs building. The
  version, the gateway's systemd unit and the SDK's protobuf modules follow
  upstream by themselves; the rest of
  [`openshell.spec`](packaging/openshell/openshell.spec), such as file lists
  and dependencies, does not. To republish a version after changing the spec,
  bump its `baserelease`.

  The same run builds `brig` and `brig-selinux` from the latest brig release
  if it is not published for every Fedora release yet; a brig release starts
  the workflow itself, so that it reaches the repository within minutes. The
  build is offline, against the Go modules that
  [`make-srpm.sh`](packaging/brig/make-srpm.sh) vendors from the release tag.
  [`brig.spec`](packaging/brig/brig.spec) lists the dependencies of `brig
  print-fedora-deps` by hand, and a test fails when they differ. A run that
  builds neither keeps the published packages. To republish a version after
  changing the spec, bump its `baserelease`, too.
- [`openshell-releases`](.github/workflows/openshell-releases.yaml) runs
  weekly and opens an `openshell-release` issue for each new release, so that
  a maintainer checks the spec and brig against it. Running
  `/openshell-release <issue>` in Claude Code works through [the
  checklist](.claude/skills/openshell-release/SKILL.md); it never starts on
  its own.

GitHub disables scheduled workflows in a public repository after 60 days
without repository activity. Each scheduled run of these two workflows
therefore enables both again, so that a quiet period does not stop them.

### Package repository setup

One-time setup for maintainers:

1. Create the signing key on a trusted machine, from a checkout of this
   repository (needs `git`, `gpg` and an authenticated `gh`): `mise run
   signing-key` or `scripts/signing-key.sh`. It restricts the `rpm-signing`
   environment to the `main` branch (removing any other deployment branch or
   tag policy it finds there), stores the secret key as its secret
   `RPM_SIGNING_KEY` and writes the public key to
   `packaging/brig-release/RPM-GPG-KEY-brig`; commit it. It also leaves an
   unencrypted backup of the secret key and its revocation certificate in
   `~/.local/share/brig/signing-key/`; move them to offline storage.
2. In the repository settings, set *Pages → Source* to *GitHub Actions*.

`RPM-GPG-KEY-brig` lists every key that clients trust; the packages workflow
signs with one of them and re-signs the published packages that another
listed key signed. To replace the key, run `scripts/signing-key.sh` with
each of these options in turn, and bump brig-release's `Version` with the
`--next` and `--retire` commits, so that clients get the new keys from a
package that a key they trust signed:

1. `--next` adds a new key beside the current one and backs it up.
2. Once clients have updated brig-release, `--switch FILE` with that
   backup makes the next publish sign everything with the new key.
3. After that publish, `--retire FINGERPRINT` drops the old key.

After a key compromise, instead run `--next`, `--switch` and `--retire` at
once, then the packages workflow with *discard-published*, which publishes
only what it rebuilds. Clients then need the new key by hand: `sudo rpm
--import https://dennisklein.github.io/brig/RPM-GPG-KEY-brig`, and `sudo
rpmkeys --delete` with the old key's ID.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). The web UI embeds
htmx (0BSD) and the IBM Plex Sans, JetBrains Mono and Bricolage Grotesque
fonts (OFL-1.1); their licenses are in
[internal/ui/web/static/licenses](internal/ui/web/static/licenses).
