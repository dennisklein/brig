# Unblock an agent

When an agent cannot reach a host, its sandbox's policy, [fence
1](../concepts/architecture.md#the-two-fences), denied the request, or the
VM's network profile, fence 2, dropped it. This page shows how to find the
denial, allow it for one sandbox, and keep the rule for sandboxes created
later. `brig help unblock` and `brig help keep-rules` have the short version.

## See what was denied

- In brig ui, Traffic lists each sandbox's requests with the rule that allowed
  a request or the reason it was denied; denials of the last hour also show on
  the Overview, under Needs you.
- In a terminal, `brig sandbox logs dev webapp --since 10m --source sandbox`,
  `openshell term`, or `brig audit dev webapp --denied -f` show the method,
  path, program and reason of each denial.

A request denied as `credential_endpoint_mismatch` carries a provider's
placeholder to a host its profile does not list. Allowing it does not help;
fix the profile's endpoints instead.

## Allow it for this sandbox

```sh
brig policy allow dev webapp registry.npmjs.org --binary /usr/bin/node
brig policy allow dev webapp --draft ch-17          # approve the rule OpenShell drafted
```

`brig policy allow` adds one rule that lets `--binary PATH` reach HOST, as
`openshell policy update` does, and records in the policy revision that brig
made it, so the [audit trail](../concepts/audit-trail.md) can say who made
the change. `--access` sets what the rule allows (read-only by default),
`--l4` allows the whole connection instead of inspecting its requests, and
`--audit` only logs what the rule would deny. It refuses `*`, `**` and
wildcards above a public suffix, such as `*.com`, `*.co.uk` or `*.github.io`,
and wants `--broad` for full access, `--l4` or a program path with a wildcard.
The sandbox loads the rule at once; no other sandbox gets it.

In brig ui, **Allow…** on a denied request does the same: it offers the rule
that OpenShell drafted from the request, which names the program's full path,
or a rule of your own with its access level and enforce or audit. A request
denied because a provider's credential may not go to its host is allowed only
after you confirm that it goes out without it. **Reject…** drops a draft that
you do not want (`openshell rule reject`).

For a rule or two by hand, `openshell policy update webapp ...` works too, and
`openshell policy set webapp --policy FILE --wait` replaces the policy with a
reviewed file.

## Keep it for new sandboxes

A rule added to a running sandbox lasts as long as the sandbox, and `brig
sandbox recreate` and `brig agent update` carry it over to the new sandbox.
A new sandbox, or one created again by hand, starts from its policy file
instead. To keep the rule for those, write it into the [policy
file](../concepts/openshell-config.md#which-policy-a-sandbox-gets) that new
sandboxes of the project are created from:

```sh
brig policy diff dev webapp              # rules the sandbox has and its file lacks, and the reverse
brig policy save dev webapp              # write them into the file, keeping its comments
brig policy allow dev webapp HOST --binary PATH --save   # add one rule and save it in one step
```

`brig policy save` shows the change and the hosts it allows and asks first on
a terminal; `--dry-run` only shows it, and `--rule KEY` saves one rule. A
sandbox created from `policies/default.yaml` gets a file of its own with `--to
webapp`, so the shared default stays without network rules. In brig ui, tick
**Keep for new sandboxes** in Allow…, or press **Save to file…** on the
sandbox's Policy tab.

## When the policy allows it

If a request fails although a rule allows it:

- **The program's path differs** from the rule's, for example `/usr/local/bin`
  instead of `/usr/bin`. `brig sandbox exec dev webapp -- sh -c 'readlink -f
  "$(command -v TOOL)"'` prints the real path; put that in the rule.
- **Fence 2 drops it.** The VM's network profile blocks for the whole VM,
  outside it, and its drops are not logged yet. The default profile blocks
  hosts on the LAN and on this host. `brig show dev` shows the profile, and
  `brig update dev --profile NAME` changes it at the next start; see [network
  profiles](../reference/network-profiles.md).
