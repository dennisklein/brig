---
title: "Gateway"
summary: "The OpenShell control plane inside a VM. Your host's openshell CLI reaches it as brig-NAME."
aliases: ["brig-NAME", "openshell gateway", "control plane"]
see: [vm, workspace, brig-use]
commands:
  - "brig use dev"
  - "openshell sandbox list"
ui: "/gateway"
doc: "concepts/architecture.md#vm"
---
Each VM runs one. It holds the VM's sandboxes, providers, profiles and
policies, and talks to the CLI on this host over mTLS on 127.0.0.1.

Each terminal picks its own gateway: `brig use NAME` points the openshell CLI
in that shell at one.
