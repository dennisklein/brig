---
title: "Host directory"
summary: "A directory on this host shared into a VM with virtiofs, read-only unless rw. A change applies to a running VM at once."
aliases: ["mount", "virtiofs", "share"]
see: [sandbox-volume]
commands:
  - "brig update dev --add-mount ~/src/kit:/srv/kit:ro"
doc: "how-to/share-directories.md#into-the-vm"
---
In the VM, user agent owns what you own on the host, and what it writes is
yours.
