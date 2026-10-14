---
title: "VM"
summary: "A Fedora VM on this host that runs one OpenShell gateway and the sandboxes on it."
aliases: ["virtual machine", "domain"]
see: [gateway, data-disk, base-image, fence-2]
commands:
  - "brig list"
  - "brig show dev"
  - "brig up dev"
ui: "/vms"
doc: "concepts/architecture.md#vm"
---
It runs rootless in your libvirt user session and shows in virt-manager as
brig-NAME, but start it with `brig start`, which also starts its network.

It boots from a throwaway overlay of a base image. Everything that must last,
such as the gateway's state, secrets and workspaces, is on its data disk.
