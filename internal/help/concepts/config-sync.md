---
title: "Sync"
summary: "Applies a VM's config directories to its gateway: profiles, providers with secrets from your keyring, and the default policy."
aliases: ["brig sync", "dry run", "prune"]
see: [config-dir, provider]
commands:
  - "brig sync dev --dry-run"
  - "brig sync dev"
doc: "concepts/openshell-config.md#what-a-sync-does"
---
Preview it with --dry-run: it lists what would change and where each secret may
be sent. A sync deletes nothing unless you pass --prune.

brig start syncs too, but holds back a change to a profile whose credential the
gateway already has, unless it only renames or describes the profile. Once the
gateway has credentials, it also holds back new providers and credentials
looked up elsewhere.
