---
title: "Services"
summary: "A workload that sandboxes use but never control, such as a model proxy. Each attached sandbox has a token of its own."
aliases: ["service", "brig service", "service vm"]
see: [provider, fence-2]
commands:
  - "brig service list"
  - "brig service attach mirror dev webapp"
doc: "concepts/services.md"
---
brig stores each sandbox's token as a provider named brig-svc-SERVICE-SANDBOX
on its gateway. Detach a sandbox with brig service detach, not by deleting
that provider: only brig service detach makes the service refuse the token.
