---
title: "Policy file"
summary: "A policy in your config directory, such as policies/webapp.yaml, that new sandboxes are created from."
aliases: ["policy save", "policies/"]
see: [base-policy, config-dir]
commands:
  - "brig policy save dev webapp --dry-run"
  - "brig policy save dev webapp"
doc: "how-to/unblock.md#keep-it-for-new-sandboxes"
---
`brig policy save` writes the rules a sandbox has and its file lacks into that
file, keeping its comments, so that sandboxes created from it get them too. It
shows the change and the hosts it opens before it writes.

A sandbox created from the default policy needs --to NAME, which creates
policies/NAME.yaml.
