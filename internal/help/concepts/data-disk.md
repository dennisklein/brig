---
title: "Data disk"
summary: "The VM's btrfs disk at /home/agent: gateway state, secrets, images and sandbox workspaces. Kept on upgrade, deleted with the VM."
see: [root-disk, base-image]
doc: "concepts/disks-and-images.md#disks"
---
Treat a VM's directory as holding its secrets: the gateway keeps them encrypted
on this disk, next to the key.
