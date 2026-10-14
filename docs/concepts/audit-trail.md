# Audit trail

The audit trail answers "what did this agent reach, and who allowed it?",
also for sandboxes that are long gone. OpenShell's gateway alone cannot: it
keeps the last lines of each sandbox's log in memory only, loses them when the
gateway restarts, and deletes a sandbox's policy history with the sandbox. So
brig records them itself, on the host, outside the VM.

## The collector

While a VM runs, a systemd user unit on the host,
`brig-audit-<vm>.service`, follows the VM's gateway and records what its
sandboxes did into `$XDG_STATE_HOME/brig/audit/<vm>/` (usually
`~/.local/state/brig/audit/<vm>/`). `brig start` starts it once the gateway
answers; `brig stop` and `brig upgrade` stop it after the VM, so it reads the
last lines before the stream ends. The unit restarts on failure, and only one
collector writes for a VM at a time.

## What it records

The files are JSON lines, one file per day, compressed once the day is over.
Every record has its time, VM and kind, and names its workspace and sandbox
where it has one:

| Kind | What it records |
| --- | --- |
| `request` | a request that a sandbox's proxy allowed or denied: method, host, port, path without its query, program, the rule that allowed it or the reason it was denied |
| `policy` | each revision of a sandbox's policy, with the rules it added, removed and changed, and who made it |
| `draft` | the rules OpenShell drafted from denied requests, and the decisions on them |
| `sandbox` | sandboxes created and deleted, with image and providers |
| `action` | the brig and brig ui commands that worked on the VM |
| `gap` | that records may be missing before it, and why |

A sandbox's first policy revision is recorded with all its network rules, so
the trail shows what a sandbox could reach from its creation.

## Who changed a policy

OpenShell stores notes with each policy revision that cannot be changed
later, but only from what the client sends. So:

- **brig's own changes are labelled exactly.** `brig policy allow`, which
  brig ui's Allow… runs too, sends who made the change with it.
- **Draft approvals are matched exactly**, by the time OpenShell recorded the
  decision, and show as the draft that was approved.
- **Anything else is "unknown (openshell CLI or another client)"**, such as a
  hand-run `openshell policy set`.

## Gaps

A collector that restarts resumes from the last record it wrote, filling in
from what the gateway still holds. When that does not reach back far enough,
or the gateway reports lost lines, the collector writes a `gap` record, so
missing traffic is visible, never silent.

## Where it goes

Changes, but not allowed requests, also go to the user journal
(`journalctl --user -u brig-audit-<vm>`); `audit.journal` in
[config.yaml](../reference/config.md) sets how much. The files stay after
`brig delete`. Nothing is deleted unless `audit.retention` is set or you run
`brig audit prune`.

## What it does not cover

- Drops by a VM's network profile, fence 2, are not recorded yet.
- The trail lives in your home directory and is not tamper-evident: it guards
  against what happens in the VM, not against your own user account.

[Look back at what a sandbox did](../how-to/audit.md) shows the commands that
read it.
