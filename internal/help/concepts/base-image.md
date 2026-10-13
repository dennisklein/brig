---
title: "Base image"
summary: "A read-only image of Fedora with OpenShell, built on this host without root. VMs boot from an overlay of it."
aliases: ["mkosi", "upgrade"]
see: [root-disk, data-disk, sandbox-image]
commands:
  - "brig image build"
  - "brig upgrade dev"
ui: "/images"
---
Upgrade moves a VM onto the newest base image and keeps its data disk. If the
gateway does not answer afterwards, brig rolls back.
