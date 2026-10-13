---
title: "Find out why a request fails that the policy allows"
concepts: [fence-2, fence-1]
commands:
  - "brig show dev"
  - "brig update dev --profile lan"
show:
  href: "/vms"
  action: profile
---
1. Open the VM, then Network. Fence 2 blocks for the whole VM, outside it, and
   its drops are not logged.
2. Hosts on the LAN or on this host are blocked by the default profile.
3. Change the profile; it applies when the VM next starts.
