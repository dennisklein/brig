---
title: "See what a sandbox did and who changed its policy"
concepts: [audit-trail, verdict, policies]
commands:
  - "brig audit dev webapp --since 7d"
  - "brig audit dev webapp --summary"
  - "brig audit dev webapp --denied -f"
show:
  href: "/traffic"
doc: "how-to/audit.md"
---
1. Open Traffic and pick 24 hours, 7 days or all instead of the last hour.
   Pick a sandbox to narrow it, a deleted one included.
2. Open the sandbox's History tab for its policy revisions with who made
   each, its drafts and the endpoints it reached.
3. In a terminal, brig audit prints the same as a timeline, and --summary
   each endpoint with the rule that allowed it.
