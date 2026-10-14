---
title: "Set up brig on a new host"
concepts: [vm, base-image, config-dir]
commands:
  - "brig doctor"
  - "brig create dev"
  - "brig update dev --add-openshell-config ~/src/agent-openshell"
show:
  href: "/"
  action: create
doc: "start/first-vm.md"
---
1. Open Host and check that every check passes.
2. Press New VM. brig builds a base image first if there is none, then creates
   and starts the VM with its gateway.
3. Add your OpenShell config directories to the VM, so that its gateway gets
   your providers and default policy.
4. Start an agent in a new sandbox.
