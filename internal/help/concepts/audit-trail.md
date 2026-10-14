---
title: "Audit trail"
summary: "brig's own history of each VM: every request allowed or denied, policy revisions and who made them, drafts, sandboxes and brig commands."
aliases: ["history", "audit log", "collector", "journal", "retention", "prune"]
see: [verdict, draft, policies, activity]
commands:
  - "brig audit dev webapp --since 7d"
  - "brig audit dev webapp --summary"
  - "brig audit status"
ui: "/host"
doc: "concepts/audit-trail.md"
---
While a VM runs, a collector on the host, the user unit brig-audit-VM, writes
it to files outside the VM, so the history stays after a sandbox or the VM is
deleted. Gaps mark times when records may be missing.

In brig ui, Traffic reads it when you pick 24 hours, 7 days or all, a
sandbox's History tab lists its revisions, drafts and the endpoints it
reached, and Host shows each collector and the space the history takes.

brig keeps all of it unless audit.retention in config.yaml is set or you
prune it.
