---
title: "Sandbox policy"
summary: "Filesystem, process and network rules for one sandbox. OpenShell picks it when the sandbox is created."
aliases: ["global policy", "default policy", "OPENSHELL_SANDBOX_POLICY"]
see: [base-policy, effective-policy, policy-file, fence-1]
commands:
  - "openshell policy get webapp"
ui: "/gateway?tab=policies"
---
It takes the first that exists: a global policy on the gateway, the file given
at create (--policy, or OPENSHELL_SANDBOX_POLICY, which brig use sets from your
config directory), /etc/openshell/policy.yaml in the image, or OpenShell's
built-in default with no network at all.

Network rules reload at once. Filesystem and process settings are fixed once
the sandbox starts.
