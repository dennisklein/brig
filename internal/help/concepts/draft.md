---
title: "Drafted rule"
summary: "A rule OpenShell drafts from a sandbox's denied requests. Approve it to add it to that sandbox, or reject it."
aliases: ["drafts", "approve", "reject"]
see: [fence-1, policy-file]
commands:
  - "brig policy allow dev webapp HOST --draft ID"
doc: "how-to/unblock.md#allow-it-for-this-sandbox"
---
Drafts name the program's full path and the endpoints it tried. OpenShell
checks whether a draft opens a new way for data to leave.

`brig up` lists the drafts that wait for review.
