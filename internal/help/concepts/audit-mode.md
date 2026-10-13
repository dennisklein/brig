---
title: "Audit mode"
summary: "A rule in audit mode lets requests pass and logs what enforcing it would deny."
aliases: ["enforce", "enforcement"]
see: [verdict, fence-1, audit-trail]
commands:
  - "brig policy allow dev webapp HOST --binary PATH --audit"
---
Use it to learn what an agent needs before you commit to a rule. It works only
for hosts that no attached provider covers, since overlapping endpoints must
agree.
