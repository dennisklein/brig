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
  sandbox images in the VM's Podman, every sandbox's workspace and its
  [snapshots](#snapshots). `brig upgrade` keeps it, `brig delete` deletes it. It is formatted btrfs at
  first boot, with zstd compression, which shrinks checkouts and
  `node_modules` at little CPU cost. The root disk stays ext4.

The data disk fills up over time: `df -h /home/agent` in `brig ssh dev` shows
how full it is, and `brig update dev --data-disk 80GiB` grows it at the next
boot. btrfs shares blocks between copies of a file, so `du` can add up to more
than the disk holds; `df` and `btrfs filesystem df /home/agent` show the real
use. After a host crash, btrfs recovers by replaying its log when it mounts the
disk; see [troubleshooting](../troubleshooting.md#files-missing-after-a-crash).

brig mounts every data disk as btrfs. A VM that brig 0.6 or earlier created
has an ext4 data disk, and newer brig cannot boot it: the mount of
`/home/agent` fails, so the VM stops in emergency mode and `brig start` gets no
SSH connection. brig does not convert the disk. Before you upgrade brig, copy
out what you need from such a VM, then `brig delete dev`, upgrade, and `brig
create dev`. If you upgraded first, `sudo dnf downgrade brig` back to 0.6, copy
out what you need, delete the VM, and upgrade again.

## Snapshots

A snapshot is a copy of one sandbox's `/sandbox` that brig keeps in the VM,
under `~/.local/share/brig/snapshots` on the data disk, as a btrfs subvolume.
The copy is a reflink: it shares every block with the workspace it came from,
so it takes seconds and almost no space when it is taken. A block that the
sandbox changes afterwards is written anew, and the snapshot keeps the old one,
so a snapshot grows as its sandbox moves on.

Because the blocks are shared, `du` counts them once for the workspace and
again for each snapshot, and adds up to more than the disk holds. `brig
sandbox snapshot list dev` shows what each snapshot holds alone, the blocks it
shares with nothing else, which is what deleting it frees. A restore and
`brig sandbox recreate` copy the snapshot back the same way.

Snapshots belong to the VM, not to a sandbox: they outlive the sandbox they
came from, and `brig delete` deletes them with the VM. [Keep agent
state](../how-to/keep-agent-state.md#snapshots) shows how to use them.

## Sandbox images

A sandbox runs a container image from the VM's Podman, which is separate from
the VM's base image. You build sandbox images on the host with Podman, or with
`brig agent build`, and copy them into the VM with `brig image push`. They live
on the data disk, so an upgrade keeps them. [Upgrade](../how-to/upgrade.md)
shows how each kind of image is rebuilt and rolled out.
