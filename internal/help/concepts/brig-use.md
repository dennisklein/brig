---
title: "Using a VM from a terminal"
summary: "brig use NAME points the openshell CLI of the current shell at a VM's gateway, and at its default sandbox policy."
aliases: ["brig use", "OPENSHELL_GATEWAY", "shell"]
see: [gateway, workspace]
commands:
  - "brig use dev"
  - "brig use"
  - "brig use --unset"
---
It needs brig's shell integration, which the package sets up. The Powerlevel10k
segment then shows the VM, such as ▥ dev.

brig ui does not change your shells. Its buttons show the command to paste
instead.
