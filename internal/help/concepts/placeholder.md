---
title: "Placeholder"
summary: "What the agent sees instead of a secret, such as openshell:resolve:env:GH_TOKEN."
aliases: ["openshell:resolve", "credential_endpoint_mismatch"]
see: [provider, provider-profile]
doc: "concepts/openshell-config.md#placeholders"
---
The sandbox's proxy swaps in the real value on the way out, only on requests
the policy allows and only to the provider's endpoints. A leaked placeholder is
worth nothing.

A request that carries a placeholder to a host the profile does not list is
denied as credential_endpoint_mismatch. Allowing that host does not help: fix
the profile's endpoints instead.
