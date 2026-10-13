---
title: "Base policy"
summary: "The policy the sandbox was created with, plus the rules added to it since, such as with Allow…."
see: [effective-policy, policy-file]
commands:
  - "openshell policy get webapp --base"
  - "brig policy diff dev webapp"
---
Rules added to a running sandbox are not in its policy file until you save
them, so a sandbox created again from that file lacks them.
