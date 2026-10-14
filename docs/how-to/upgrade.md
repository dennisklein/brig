# Upgrade

The VM's base image and the sandbox image move independently:

| Image | Contains | Rebuild | Roll out | Existing sandboxes |
| --- | --- | --- | --- | --- |
| VM base image | Fedora, OpenShell gateway, Podman | `brig image build` | `brig upgrade dev` | kept, with their workspaces |
| Sandbox image | Pi and your kit, or Claude Code and your skills; gh, glab, git | `podman build --pull` | `brig image push dev IMAGE` | keep their old image until recreated |
| OpenShell CLI on the host | `openshell` | `sudo dnf upgrade openshell` | immediate | unaffected |

## VM and OpenShell

brig's package repository picks up each OpenShell release within a day.
Upgrade in this order:

1. `brig image build` builds a base image with the newest Fedora packages and
   OpenShell; pin with `--openshell 0.1.4` to wait.
2. `brig upgrade dev` moves the VM onto it and rolls back if its gateway does
   not answer, or on Ctrl-C. The data disk, and with it every provider, template, image and
   workspace, stays. If OpenShell's minor version changes, brig refuses
   without `--allow-minor`: read the release notes first, since sandboxes may need
   to be recreated and the profile or policy schema may have changed.
3. `sudo dnf upgrade openshell` on the host. `brig start` warns when the CLI's
   and the gateway's minor versions differ.

dnf in the VM does not upgrade OpenShell: its packages are excluded in the
image, so `brig upgrade` is the only way to move the gateway.

`brig image prune` then removes base images no VM uses, keeping the newest,
and the packages that mkosi cached for builds in `~/.cache/brig/mkosi`.

## Sandbox image

Rebuild on a schedule, say weekly, with `--pull` for the base image's security
fixes. The version pins cover only the packages the Containerfile names. Pi
1.1.0 ships no `npm-shrinkwrap.json`, so npm resolves its own core packages,
`@earendil-works/pi-ai`, `pi-agent-core` and others, from `^1.1.0` ranges on
every build. With `--pull`, a new base image also reruns the npm install, so a
rebuild without edits can install newer Pi core packages. Change versions on
purpose: bump `PI_VERSION`, `CLAUDE_VERSION` or `OPENCODE_VERSION` in the Containerfile, and read
Pi's changelog before you move sandboxes to the new image.

```sh
brig agent build dev pi
```

It runs `podman build --pull -t localhost/pi-agent:DATE agents/pi/` (the date
gets `-2` and so on if that tag exists), copies the image into the VM unless it
has it already, and deletes and creates the template `pi` for the new tag:
OpenShell can only create and delete templates. `--no-pull` keeps the base
image, `--tag` sets the tag.

New sandboxes get the new image. Move an existing one when you are at a good
point, following the [habits above](keep-agent-state.md#habits-that-make-deletes-safe); nothing
forces a move.

Old images stay in the VM until you remove them, in `brig ssh dev`: `podman
images`, then `podman rmi localhost/pi-agent:OLD` for tags no sandbox uses.
Podman refuses to remove an image a container still uses.
