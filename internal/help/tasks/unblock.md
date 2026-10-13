---
title: "Unblock an agent that cannot reach a host"
concepts: [fence-1, draft, audit-mode, policy-file]
commands:
  - "brig policy allow dev webapp registry.npmjs.org --binary /usr/bin/node"
  - "brig policy save dev webapp"
show:
  href: "/traffic?verdict=deny"
  action: allow
---
1. Open Traffic and pick Denied, or the sandbox's Traffic tab.
2. Press Allow… on the request. If OpenShell drafted a rule for it, approve the
   draft; otherwise the dialog fills in a rule from the request: host, port and
   program.
3. Keep the access level as small as works, and pick Audit to watch first if
   you are unsure. The sandbox loads the rule at once.
4. If sandboxes created later need the rule too, save it to the policy file.

A request that is denied as credential_endpoint_mismatch carries a provider's
placeholder to a host its profile does not list. Allowing it does not help; fix
the profile's endpoints instead.
