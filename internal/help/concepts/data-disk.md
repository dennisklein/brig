---
title: "Data disk"
summary: "The VM's btrfs disk at /home/agent: gateway state, secrets, images, sandbox workspaces and snapshots. Kept on upgrade, deleted with the VM."
see: [root-disk, base-image, snapshot]
doc: "concepts/disks-and-images.md#disks"
---
Treat a VM's directory as holding its secrets: the gateway keeps them encrypted
on this disk, next to the key.

Snapshots share blocks with the workspaces they came from, so du overcounts
them; brig sandbox snapshot list shows what each holds alone.
