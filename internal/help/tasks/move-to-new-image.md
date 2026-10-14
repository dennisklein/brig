---
title: "Move sandboxes to a new agent image"
concepts: [recreate, snapshot, sandbox-image, template]
commands:
  - "brig agent build dev claude"
  - "brig agent update dev claude --dry-run"
  - "brig agent update dev claude"
doc: "how-to/upgrade.md#sandbox-image"
---
1. Run brig agent build to build the agent's image and point its template at
   it. New sandboxes get it; existing ones keep theirs.
2. Run brig agent update with --dry-run. It names what each move would stop,
   such as a running agent, and the policy rules a sandbox would carry over.
3. Run brig agent update. Each sandbox is recreated on the new image with its
   /sandbox and keeps a snapshot from before the move.

If a step fails, brig puts that sandbox back as it was and stops; the
sandboxes after it are left alone. --reset PATH lets the new image set up a
file under /sandbox fresh.
