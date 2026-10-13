---
title: "Workspace"
summary: "A tenant inside a gateway, with its own sandboxes, providers and profiles. Most setups only have default."
aliases: ["tenant", "-w"]
see: [gateway, sandboxes]
commands:
  - "brig use dev -w team"
  - "brig sandbox list dev"
---
Commands that work in one take -w. `brig use dev -w team` makes team the
default for that shell, also for plain openshell commands.

Not to be confused with /sandbox, the directory where a sandbox keeps its
files.
