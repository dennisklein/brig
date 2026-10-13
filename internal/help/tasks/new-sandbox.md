---
title: "Start an agent in a new sandbox"
concepts: [sandboxes, provider, sandbox-volume, policies]
commands:
  - "brig sandbox create dev --connect -- --name webapp --provider github"
show:
  href: "/sandboxes"
  action: sandbox-create
---
1. Press New sandbox and pick the VM, a name and a template or image.
2. Attach only the providers the agent needs, and the host directories it may
   read.
3. Create it, then connect from your terminal and start the agent inside.
