---
title: "Sync"
summary: "Applies a VM's config directories to its gateway: profiles, providers with secrets from your keyring, and the default policy."
aliases: ["brig sync", "dry run", "prune"]
see: [config-dir, provider]
commands:
  - "brig sync dev --dry-run"
  - "brig sync dev"
---
Preview it with --dry-run: it lists what would change and where each secret may
be sent. A sync deletes nothing unless you pass --prune.

brig start syncs too, but holds back a profile change that adds endpoints for a
credential the gateway already has.
