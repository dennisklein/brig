---
title: "Let an agent test in a throwaway VM"
concepts: [spike-vm, sandboxes]
commands:
  - "brig service create vms --type vm"
  - "brig service attach vms dev webapp"
  - "brig vm-runner list"
doc: "how-to/test-in-vms.md"
---
1. Name the images that agents may start in vm_runner.images in
   config.yaml: qcow2 files with qemu-guest-agent.
2. Create a vm runner service and attach it to the sandbox. Add its MCP
   server to the agent.
3. Watch the VMs with brig vm-runner list or the Spike VMs page of brig ui;
   brig vm-runner destroy deletes one.
