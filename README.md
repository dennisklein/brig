# brig

`brig` manages the lifecycle of agent-sandbox VMs on a Fedora laptop. Each VM
runs headless Fedora with an [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell)
gateway, so autonomous agents are contained by a VM boundary in addition to
OpenShell's own sandboxing. Your host's `openshell` CLI drives each VM's
gateway; brig registers it as `brig-<vm>`.

> **Status:** early development. Expect breaking changes.

## How it works

- **VMs** run rootless in your libvirt user session (`qemu:///session`) with
  UEFI, KVM and passt user-mode networking. They show up in virt-manager as
  `brig-<vm>`, but start them with `brig start`, which also starts their
  network.
- **Base images** are built on your laptop with [mkosi](https://github.com/systemd/mkosi),
  without root: minimal Fedora plus OpenShell from brig's
  [signed package repository](#openshell-packages).
- **Disks:** each VM boots from a disposable overlay of a read-only base image.
  Everything that must survive, including OpenShell's state, credentials and
  container storage, lives on a separate data disk mounted at `/home/agent`.
  `brig upgrade` moves a VM onto a newer base image and keeps the data disk.
- **Configuration** of each boot (SSH keys, hostname, mounts) is passed as
  systemd credentials. There is no cloud-init.
- **Network profiles** restrict what a VM may reach: the internet, the LAN,
  services on the host. They are enforced outside the VM, so even root in the
  VM cannot lift them: each VM's passt runs in a private network namespace,
  connected to the host by pasta, whose nftables rules brig sets up without
  root privileges. This adds to OpenShell's own per-sandbox egress policy.
- **Provider secrets** never pass through brig. Create OpenShell providers
  with your host's `openshell` CLI; it sends them straight to the VM's
  gateway. brig only copies the gateway's mTLS client certificate and key
  into the CLI's configuration.

## Install

With [mise](https://mise.jdx.dev):

```sh
mise use -g github:dennisklein/brig
```

Or with Go 1.26 or newer:

```sh
go install github.com/dennisklein/brig@latest
```

Then install what brig needs on the host and check the result:

```sh
sudo dnf install $(brig print-fedora-deps)
brig doctor
```

Add `--with mounts` to `print-fedora-deps` for `--mount` (virtiofsd), and
`--with push` for `brig image push` (Podman), or `--with all` for both.

The host also needs the `openshell` CLI, matching the VMs' OpenShell version:

```sh
sudo rpm --import https://dennisklein.github.io/brig/RPM-GPG-KEY-brig
sudo dnf install https://dennisklein.github.io/brig/brig-release.noarch.rpm
sudo dnf install --setopt=install_weak_deps=False openshell
```

## Quick start

```sh
brig create dev                     # builds a base image first if needed
brig list
eval $(brig env dev)                # point the openshell CLI at the VM's gateway

# Credentials go from your shell straight to the VM's gateway. A gateway needs
# the provider profile (a type definition, no secrets) first.
openshell profile import --global \
  --url https://raw.githubusercontent.com/NVIDIA/OpenShell/v0.1.2/providers/anthropic.yaml
ANTHROPIC_API_KEY=... openshell provider create --name anthropic --type anthropic --from-existing

# Sandboxes run container images. OpenShell's default image has no agent
# CLIs, so build your own on the host and push it into the VM.
podman build -t localhost/my-agent:latest .
brig image push dev localhost/my-agent:latest
openshell sandbox create --from localhost/my-agent:latest --provider anthropic -- my-agent

brig ssh dev                        # shell as the gateway's user
brig stop dev
brig delete dev
```

Share host directories with `--mount SOURCE[:TARGET][:OPTIONS]`, where
OPTIONS is a comma-separated list of `ro` (the default), `rw` and `sandbox`,
e.g. `--mount ~/src:/work:ro,sandbox`. With `sandbox`, OpenShell sandboxes
can use the directory too, read-only, as a Podman volume named after the
mount's tag (`brig show` lists it, e.g. `brig0`):

```sh
openshell sandbox create --driver-config-json \
  '{"podman":{"mounts":[{"type":"volume","source":"brig0","target":"/sandbox/src"}]}}' ...
```

Container images built on the host get into a VM with
`brig image push dev localhost/my-agent:latest`.

## Commands

| Command | Purpose |
|---|---|
| `create NAME` | create and start a VM (`--cpus`, `--memory`, `--root-disk`, `--data-disk`, `--profile`, `--mount`, `--image`) |
| `list`, `show NAME` | list VMs, show one (`-o json`) |
| `start NAME`, `stop NAME` | boot (and re-register the gateway), shut down (`--force` powers off) |
| `update NAME` | change CPUs, memory, profile and mounts, grow disks (all at the next boot) |
| `upgrade NAME` | move onto the newest base image, rolling back on failure |
| `delete NAME` | delete the VM, its disks and its gateway registration |
| `ssh NAME`, `console NAME` | shell (`--root` for root), serial console |
| `env NAME` | print `export OPENSHELL_GATEWAY=brig-NAME` |
| `image build/list/rm/prune` | manage base images |
| `image push NAME IMAGE` | copy a container image from the host's Podman into a VM |
| `doctor`, `print-fedora-deps` | check host prerequisites, list the Fedora packages they need |
| `completion SHELL` | shell completion for bash, zsh, fish and PowerShell |

## Configuration

`$XDG_CONFIG_HOME/brig/config.yaml` (usually `~/.config/brig/config.yaml`)
overrides the built-in defaults shown here and can add network profiles,
such as `ollama` below. A profile in the file replaces a built-in profile of
the same name as a whole.

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
  # Not built in: an example of your own profile.
  ollama:               # internet plus a model server on the host
    internet: true
    host_ports: [11434]
```

The host is reachable from a VM at the VM's default gateway address; with
`host_ports`, only those TCP ports are. Without `host`, the host's own
addresses are blocked too. The LAN means the private, shared (100.64.0.0/10),
link-local, multicast and broadcast address ranges plus the networks of the
host's addresses, which brig reads when a VM starts: restart VMs after
joining another network.

DNS queries go to the host's resolver whenever a profile allows the
internet, the LAN or the host, so a VM can then look up any name, even
without internet access. Profiles that allow none of these, such as
`isolated` or one with only `host_ports`, get no DNS.

A VM's gateway pulls OpenShell's container images from ghcr.io and nvcr.io
and refreshes provider tokens itself, so profiles without `internet` need
images pushed into the VM.

## OpenShell packages

brig's VM images, and your host, get OpenShell from
<https://dennisklein.github.io/brig/>, a dnf repository that the
[packages workflow](.github/workflows/packages.yaml) fills daily: it builds
each new OpenShell release from source for the Fedora releases listed in
[`packaging/fedora-releases`](packaging/fedora-releases), signs packages and
metadata, and publishes them to GitHub Pages. See [`packaging/`](packaging/).

One-time setup for maintainers:

1. Create the signing key on a trusted machine, from a checkout of this
   repository (needs `git`, `gpg` and an authenticated `gh`):
   `mise run signing-key` or `scripts/signing-key.sh`. It restricts the
   `rpm-signing` environment to the `main` branch, stores the secret key as
   its secret `RPM_SIGNING_KEY` and writes the public key to
   `packaging/brig-release/RPM-GPG-KEY-brig`; commit it. It also leaves an
   unencrypted backup of the secret key and its revocation certificate in
   `~/.local/share/brig/signing-key/`; move them to offline storage.
2. In the repository settings, set *Pages → Source* to *GitHub Actions*.

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

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
