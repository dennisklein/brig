---
title: "Provider"
summary: "A named credential on a gateway, such as a token. The agent sees a placeholder; the proxy adds the real value, only for the profile's endpoints."
aliases: ["credential", "token", "secret", "api key"]
see: [provider-profile, placeholder, config-sync, config-dir]
commands:
  - "brig sandbox attach dev webapp github"
  - "brig secret set dev github"
  - "brig sync dev"
ui: "/gateway?tab=providers"
---
Providers come from your config directories on sync, with their secrets read
from your keyring or a BRIG_SECRET_ variable, or are created by hand with openshell provider create.

Attaching one also adds a network rule named _provider_NAME to the sandbox's
effective policy, which opens the profile's endpoints for its binaries.

brig ui never shows or takes a secret. brig secret set stores one in your
keyring and syncs that provider; brig secret list shows which are missing.
