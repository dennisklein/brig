---
title: "Spike VM"
summary: "A throwaway VM that an agent started on this host through a vm runner service, to test on a whole OS. It has no network."
aliases: ["vm runner", "vm_create", "throwaway vm"]
see: [services, sandboxes, base-image]
commands:
  - "brig vm-runner list"
  - "brig vm-runner screenshot 3f9c1a2e"
  - "brig vm-runner destroy 3f9c1a2e"
doc: "how-to/test-in-vms.md"
---
Agents start spike VMs with the MCP tools of a vm runner service, from the
images that vm_runner.images in config.yaml allows, and drive them with
screenshots, clicks, typing and commands through the guest agent. A sandbox
sees only its own. brig destroys them when their lifetime ends or the
sandbox loses its token.
