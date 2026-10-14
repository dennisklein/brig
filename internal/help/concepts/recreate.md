---
title: "Recreate"
summary: "Move a sandbox to a new image: snapshot it, create it again with the same name, restore /sandbox. brig agent update does it for an agent."
aliases: ["agent update", "move sandbox", "new image"]
see: [sandboxes, snapshot, sandbox-image, template]
commands:
  - "brig agent update dev claude --dry-run"
  - "brig sandbox recreate dev webapp"
  - "brig sandbox recreate dev webapp --image localhost/pi-agent:2026-10-14"
doc: "how-to/upgrade.md#sandbox-image"
---
OpenShell cannot change a sandbox's image, so brig deletes the sandbox and
creates it again with the same name, providers, labels, command, services and
base policy, then restores its /sandbox from a snapshot. The new sandbox has a
new ID and SSH host key, and its policy history starts over.

If a step fails, brig puts the old sandbox back on its old image. The snapshot
stays until you prune it.
