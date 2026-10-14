---
title: "Fence 2, the network profile"
summary: "The VM's network profile, enforced by nftables on the host, outside the VM. Root in the VM cannot lift it."
aliases: ["fence 2", "network profile", "nftables", "lan", "internet"]
see: [fence-1, vm]
commands:
  - "brig update dev --profile default"
  - "brig show dev"
doc: "concepts/architecture.md#network-profiles-in-detail"
---
A profile allows or blocks the internet, the LAN, services on this host and
IPv6, for the whole VM. A change applies when the VM next starts.

Drops at fence 2 are not logged yet, so a request that fence 1 allows can still
fail here without a trace in Traffic.
