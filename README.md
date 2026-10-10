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
> brig runs on x86_64 Fedora hosts only.

## Install

With [mise](https://mise.jdx.dev):

```sh
mise use -g github:dennisklein/brig
```

Or with Go 1.26 or newer:

```sh
go install github.com/dennisklein/brig@latest
```

Then install what brig needs on the host, including the `openshell` CLI from
brig's [package repository](#package-repository), which must match the VMs'
OpenShell version, and check the result:

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
eval "$(brig env dev)"          # point the openshell CLI at the VM's gateway
openshell sandbox create        # a shell in OpenShell's default image
brig ssh dev                    # a shell in the VM, as the gateway's user
brig stop dev
brig delete dev
```

To run an agent, build an image for it and give it credentials as described in
[the guide](docs/openshell.md#your-first-agent-sandbox-pi).

Share host directories with `--mount SOURCE[:TARGET][:OPTIONS]`, where OPTIONS
is a comma-separated list of `ro` (the default), `rw` and `sandbox`. With
`sandbox`, OpenShell sandboxes can attach the directory read-only as a Podman
volume named after the mount's tag, which `brig show` lists (e.g. `brig0`);
the guide has an
[example](docs/openshell.md#developing-plugins-without-rebuilding).

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

## Commands

| Command | Purpose |
|---|---|
| `create NAME` | create and start a VM |
| `list`, `show NAME` | list VMs, show one (`-o json`) |
| `start NAME`, `stop NAME` | boot and re-register the gateway, shut down |
| `update NAME` | change CPUs, memory, disks, network profile and mounts (at the next boot) and OpenShell config directories |
| `sync NAME` | apply the VM's OpenShell config directories to its gateway |
| `upgrade NAME` | move onto the newest base image, keeping the data disk and rolling back on failure |
| `delete NAME` | delete the VM, its disks and its gateway registration |
| `ssh NAME`, `console NAME` | shell (`--root` for root), serial console |
| `env NAME` | print `export OPENSHELL_GATEWAY=brig-NAME` and the path of the default sandbox policy as `OPENSHELL_SANDBOX_POLICY`; use as `eval "$(brig env NAME)"` |
| `image build/list/rm/prune` | manage base images |
| `image push NAME IMAGE` | copy a container image from the host's Podman into a VM |
| `doctor`, `print-fedora-deps` | check host prerequisites, list the Fedora packages they need |
| `completion SHELL` | shell completion for bash, zsh, fish and PowerShell |

`brig COMMAND --help` lists each command's flags.

## Configuration

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
secrets brig looks up in your keyring with `secret-tool` (`providers/*.yaml`,
which cannot hold values), and a default sandbox policy
(`policies/default.yaml`). brig applies a VM's directories to its gateway at
every `brig start` and on `brig sync NAME`.

Attach directories with `brig create --openshell-config DIR` or `brig update
--add-openshell-config DIR`, or list defaults for new VMs in
`openshell.configs`. A provider's secrets may be sent to every endpoint of its
profile, so check a shared profile before you pair it with your own secrets;
`brig sync --dry-run` names the endpoints. Without `--prune`, brig never
deletes anything. [The guide](docs/openshell.md#openshell-config-directories)
has a worked example, and `brig sync --help` the rules.

## Package repository

brig's VM images, and your host, get OpenShell from
<https://dennisklein.github.io/brig/>, a signed dnf repository that the
[packages workflow](.github/workflows/packages.yaml) fills daily: it builds
each new OpenShell release from source for the Fedora releases listed in
[`packaging/fedora-releases`](packaging/fedora-releases). See
[`packaging/`](packaging/) and [OpenShell package
updates](#openshell-package-updates).

## Development

Tool versions are pinned in `mise.toml`:

```sh
mise install
go test ./...
golangci-lint run ./...
goreleaser release --snapshot --clean   # local release dry run
```

### Releasing

Releases are cut from annotated tags; the tag message becomes the release
notes:

```sh
git tag -a v0.1.0      # write the release notes in the editor
git push origin v0.1.0
```

### OpenShell package updates

Two workflows keep the [package repository](#package-repository) current:

- [`packages`](.github/workflows/packages.yaml) runs daily, on pushes to
  `main` that change `packaging/`, and on demand. If upstream's latest
  OpenShell release is not published yet, it builds `openshell`,
  `openshell-gateway`, `openshell-prover` and `python3-openshell` from the
  release's source, signs them, smoke-tests the repository and only then
  publishes it, keeping the previous version. A push republishes the site even when nothing needs
  building. The version, the gateway's systemd unit and the SDK's protobuf
  modules follow upstream by themselves; the rest of
  [`openshell.spec`](packaging/openshell/openshell.spec), such as file lists
  and dependencies, does not. To republish a version after changing the spec,
  bump its `baserelease`.
- [`openshell-releases`](.github/workflows/openshell-releases.yaml) runs
  weekly and opens an `openshell-release` issue for each new release, so that
  a maintainer checks the spec and brig against it. Running
  `/openshell-release <issue>` in Claude Code works through [the
  checklist](.claude/skills/openshell-release/SKILL.md); it never starts on
  its own.

### Package repository setup

One-time setup for maintainers:

1. Create the signing key on a trusted machine, from a checkout of this
   repository (needs `git`, `gpg` and an authenticated `gh`): `mise run
   signing-key` or `scripts/signing-key.sh`. It restricts the `rpm-signing`
   environment to the `main` branch, stores the secret key as its secret
   `RPM_SIGNING_KEY` and writes the public key to
   `packaging/brig-release/RPM-GPG-KEY-brig`; commit it. It also leaves an
   unencrypted backup of the secret key and its revocation certificate in
   `~/.local/share/brig/signing-key/`; move them to offline storage.
2. In the repository settings, set *Pages → Source* to *GitHub Actions*.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
