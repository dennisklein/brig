---
title: "Config directory"
summary: "A directory on this host with provider profiles, providers and policies, which sync applies to a VM's gateway."
aliases: ["openshell config", "providers/", "profiles/"]
see: [config-sync, provider, policy-file]
commands:
  - "brig update dev --add-openshell-config ~/src/agent-openshell"
doc: "concepts/openshell-config.md#config-directories"
---
Keep the shared one, such as ~/src/agent-openshell, in git. Secrets are not in
it: provider files name entries in your keyring.
