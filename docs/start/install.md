# Install brig

brig runs on x86_64 Fedora. It needs KVM, libvirt's user session, passt and
the `openshell` CLI in the same version as the VMs' gateways; `brig doctor`
checks all of it.

## From the package repository

On Fedora, the recommended way is brig's [package
repository](#the-package-repository), which also has the `openshell` CLI:

```sh
sudo rpm --import https://dennisklein.github.io/brig/RPM-GPG-KEY-brig
sudo dnf install https://dennisklein.github.io/brig/brig-release.noarch.rpm
sudo dnf install brig
brig doctor
```

The `brig` package requires everything brig needs on the host, including the
`openshell` CLI and, with SELinux policy, the `brig-selinux` package (see
[troubleshooting](../troubleshooting.md#passt-cannot-write-uid_map)). It
recommends virtiofsd for `--mount`, Podman for `brig image push` and
libsecret's secret-tool for providers in [OpenShell config
directories](../concepts/openshell-config.md#config-directories); `dnf install
--setopt=install_weak_deps=False brig` skips them.

## With mise or Go

Install the binary with [mise](https://mise.jdx.dev):

```sh
mise use -g github:dennisklein/brig
```

Or with Go 1.26 or newer:

```sh
go install github.com/dennisklein/brig@latest
```

Then install what brig needs on the host, including the `openshell` CLI from
the package repository, and check the result:

```sh
sudo rpm --import https://dennisklein.github.io/brig/RPM-GPG-KEY-brig
sudo dnf install https://dennisklein.github.io/brig/brig-release.noarch.rpm
sudo dnf install $(brig print-fedora-deps)
sudo dnf install --setopt=install_weak_deps=False openshell
brig doctor
```

Add `--with mounts` to `print-fedora-deps` for `--mount` (virtiofsd), `--with
push` for `brig image push` (Podman), `--with secrets` for providers in config
directories (secret-tool), or `--with all` for all of them.

## Shell integration

The `openshell` CLI talks to the gateway that `OPENSHELL_GATEWAY` names, so
each shell picks its VM. The brig package loads brig's shell integration in
new interactive bash and zsh sessions (`/etc/profile.d/brig.sh`) and fish
sessions: open a new terminal after installing it. Otherwise load it from your
shell's startup file to switch with `brig use NAME`:

```sh
eval "$(brig shell-init bash)"    # ~/.bashrc
eval "$(brig shell-init zsh)"     # ~/.zshrc
brig shell-init fish | source     # ~/.config/fish/config.fish
```

It defines a `brig` shell function that runs `brig use NAME` and applies what
it prints, as `eval "$(brig env NAME)"` would; `brig use --unset` points the
CLI at no VM. Every other command runs brig as before. Without the
integration, run `eval "$(brig env dev)"` instead of `brig use dev`.

`brig use dev -w team` also picks a [workspace](../concepts/workspaces.md) for
the shell.

### Prompt

In zsh the integration also defines a
[Powerlevel10k](https://github.com/romkatv/powerlevel10k) segment named
`brig`, which shows the VM the shell points at, such as `▥ dev`. Add `brig` to
`POWERLEVEL9K_LEFT_PROMPT_ELEMENTS` or `POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS` in
`~/.p10k.zsh`, and style it like other segments, for example:

```zsh
typeset -g POWERLEVEL9K_BRIG_FOREGROUND=208                   # the default
typeset -g POWERLEVEL9K_BRIG_VISUAL_IDENTIFIER_EXPANSION='VM'  # instead of ▥
```

Other prompts can show `${OPENSHELL_GATEWAY#brig-}` when `OPENSHELL_GATEWAY`
starts with `brig-`.

## The package repository

brig's VM images, and your host, get OpenShell from
<https://dennisklein.github.io/brig/>, a signed dnf repository. It has each
OpenShell release and each brig release, built from source for the Fedora
releases brig supports, and picks up a new OpenShell release within a day.
[Packaging](../contributing/packaging.md) describes how it is built.

## Next

[Create your first VM](first-vm.md).
