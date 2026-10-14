---
title: "Find out why a request fails that the policy allows"
concepts: [fence-2, fence-1]
commands:
  - "brig audit dev --kind drop"
  - "brig show dev"
  - "brig update dev --profile lan"
show:
  href: "/vms"
  action: profile
doc: "how-to/unblock.md#when-the-policy-allows-it"
---
1. Look for a fence 2 drop in Traffic, or an allowed request marked as likely
   dropped. Fence 2 blocks for the whole VM, outside it.
2. Hosts on the LAN or on this host are blocked by the default profile.
3. Profile… on the drop, or Network on the VM, changes the profile; it applies
   when the VM next starts.
