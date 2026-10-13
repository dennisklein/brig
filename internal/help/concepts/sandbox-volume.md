---
title: "Sandbox volume"
summary: "A host directory offered to sandboxes as a read-only Podman volume, named after its tag, such as brig0."
aliases: ["volume"]
see: [mount, sandboxes]
commands:
  - "brig sandbox create dev --mount ~/src/kit:/usr/local/share/kit -- --name kit"
---
`brig sandbox create --mount` does both steps: it gives the VM the mount if it
lacks it, then creates the sandbox with the volume attached.
