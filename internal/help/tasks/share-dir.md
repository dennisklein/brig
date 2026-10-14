---
title: "Share a host directory with a sandbox"
concepts: [mount, sandbox-volume]
commands:
  - "brig sandbox create dev --mount ~/src/kit:/srv/kit -- --name kit"
show:
  href: "/vms"
  action: mount-add
doc: "how-to/share-directories.md"
---
1. Open the VM, then Mounts, and press Add mount.
2. Tick Offer to sandboxes. The directory becomes a read-only Podman volume,
   such as brig0.
3. Pick it under Host directories when you create the sandbox.

brig sandbox create --mount does all of this in one command.
