---
title: "Sandbox"
summary: "A container on a gateway where an agent runs, fenced by its policy. Its directory /sandbox survives stop and start."
aliases: ["container", "agent"]
see: [policies, provider, template, fence-1, snapshot, recreate]
commands:
  - "brig sandbox create dev --connect"
  - "brig sandbox connect dev webapp"
ui: "/sandboxes"
doc: "concepts/architecture.md#sandbox"
---
A sandbox runs an image or a template, under a policy. Attached providers give
it credentials it only ever sees as placeholders.

Stopping ends every program in it but keeps /sandbox. Recreating it on a new
image keeps /sandbox too. Deleting removes /sandbox; sandbox delete --snapshot
keeps a snapshot of it first.
