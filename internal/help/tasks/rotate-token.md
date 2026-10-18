---
title: "Rotate a provider's token"
concepts: [provider, config-sync, config-dir]
commands:
  - "brig secret set dev github"
  - "brig secret list dev"
show:
  href: "/gateway?tab=providers"
doc: "how-to/secrets.md#rotate-a-token"
---
1. Run brig secret set with the VM and the provider. It asks for the new
   value without echo and stores it in your keyring under application brig
   and the attributes the provider file names.
2. If the VM runs, it syncs that provider at once. Programs that a sandbox
   starts afterwards get the new token; running agents keep the old one.

brig ui never shows or takes a secret, so this happens in your terminal.
brig secret list shows which secrets are stored and which are missing.
