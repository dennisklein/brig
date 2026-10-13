---
title: "Fence 1, the sandbox policy"
summary: "Each sandbox's policy decides, per host, program, method and path, what may leave. Changes reload at once."
aliases: ["fence 1", "proxy", "egress"]
see: [fence-2, verdict, draft, policies]
commands:
  - "brig policy allow dev webapp registry.npmjs.org --binary /usr/bin/node"
ui: "/traffic"
---
Every denial in Traffic comes from here. Allow… turns one into a rule for that
sandbox only.

A request leaves the host only if fence 1 and fence 2 both allow it.
