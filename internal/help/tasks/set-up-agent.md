---
title: "Set up Claude Code, Pi or opencode"
concepts: [sandbox-image, template, provider, provider-profile, config-dir]
commands:
  - "brig agent init claude --dir ~/src/agent-openshell --vm dev"
  - "brig agent build dev claude"
  - "brig agent update dev claude"
  - "brig sandbox create dev --connect -- --name webapp --template claude --provider claude-code"
show:
  href: "/gateway?tab=templates"
doc: "start/first-agent.md"
---
1. Run brig agent init with the agent's kind. It writes the image definition,
   provider profile, provider and a default policy into a config directory,
   and never overwrites a file.
2. With --vm it also attaches the directory, asks for missing secrets, syncs
   and builds the image. Without, it prints those commands for you to review
   and run.
3. Start a sandbox from the agent's template with its providers.

After you change the agent's files, brig agent build builds a new image and
points the template at it, and brig agent update moves the agent's sandboxes
to it.
