---
title: "Provider profile"
summary: "A kind of provider: which env vars hold the credential, how it is sent, to which endpoints and from which binaries."
aliases: ["profile", "endpoints", "binaries"]
see: [provider, effective-policy]
commands:
  - "openshell profile list"
ui: "/gateway?tab=profiles"
---
A provider's secret may be sent to every endpoint of its profile, so read a
shared profile's endpoints before you pair it with your token. `brig sync
--dry-run` lists them.
