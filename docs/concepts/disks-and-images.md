# Disks and images

A VM boots from a read-only base image and keeps everything that must last on
a separate data disk. That split is what lets `brig upgrade` replace the
operating system and OpenShell without touching your sandboxes.

## Base images

Base images are built on your laptop with
[mkosi](https://github.com/systemd/mkosi), without root: minimal Fedora plus
OpenShell from brig's [package
repository](../start/install.md#the-package-repository). `brig image build`
builds one, `brig image list` lists them, and `brig image prune` removes those
no VM uses, keeping the newest, along with the packages that mkosi cached for
builds in `~/.cache/brig/mkosi`.

dnf in the VM does not upgrade OpenShell: its packages are excluded in the
image, so a new base image and `brig upgrade` are the only way to move the
gateway.

## Disks

- **Root disk.** Each VM boots from a disposable overlay of its base image.
  `brig upgrade` replaces it; nothing on it is meant to last.
- **Data disk.** Everything that must survive lives on a separate disk
  mounted at `/home/agent`: OpenShell's state, the providers' secrets, the
  sandbox images in the VM's Podman and every sandbox's workspace. `brig
  upgrade` keeps it, `brig delete` deletes it. It is formatted btrfs at
  first boot, with zstd compression, which shrinks checkouts and
  `node_modules` at little CPU cost. The root disk stays ext4.

The data disk fills up over time: `df -h /home/agent` in `brig ssh dev` shows
how full it is, and `brig update dev --data-disk 80GiB` grows it at the next
boot. btrfs shares blocks between copies of a file, so `du` can add up to more
than the disk holds; `df` and `btrfs filesystem df /home/agent` show the real
use. After a host crash, btrfs recovers by replaying its log when it mounts the
disk; see [troubleshooting](../troubleshooting.md#files-missing-after-a-crash).

A VM created before the data disk became btrfs keeps its ext4 disk. brig does
not convert it: copy out what you need, then `brig delete dev && brig create
dev`.

## Sandbox images

A sandbox runs a container image from the VM's Podman, which is separate from
the VM's base image. You build sandbox images on the host with Podman, or with
`brig agent build`, and copy them into the VM with `brig image push`. They live
on the data disk, so an upgrade keeps them. [Upgrade](../how-to/upgrade.md)
shows how each kind of image is rebuilt and rolled out.
