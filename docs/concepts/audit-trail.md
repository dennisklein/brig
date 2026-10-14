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
| `drop` | packets that the VM's network profile, fence 2, dropped within a few seconds: protocol, address, port, how many, and the setting that dropped them |
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

## Drops at fence 2

Fence 2 is the VM's [network profile](../reference/network-profiles.md),
enforced by nftables on the host, outside the VM. Each packet it drops goes
through a chain for its reason, which counts it and logs it, up to 20 packets
a second. A listener beside passt, in the VM's network namespace, sums them
up per destination every 5 seconds into a spool in the VM's audit directory,
and the collector reads the spool into `drop` records. `brig stop` reads the
rest once the VM's network is down.

| Reason | Dropped because |
| --- | --- |
| `internet` | internet is off; under a profile that allows nothing, such as `isolated`, this covers DNS too |
| `lan` | lan is off |
| `host` | host is off |
| `host-port` | the port is not in host_ports |
| `ipv6` | ipv6 is off |
| `dns` | only plain DNS goes to the resolver |
| `udp-flows` | too many UDP flows at once |

- **Fence 2 sees packets from the VM, not from sandboxes**, so a drop names
  no sandbox. `brig audit` and brig ui match it with the requests that fence 1
  allowed to the same port just before, and name the sandbox it likely came
  from. A drop that matches none came from the VM itself, or from a
  sandbox's connection that an `--l4` rule allows: OpenShell does not log
  those.
- **What it cannot itemise, it counts.** Packets over the logging rate are
  recorded as one drop per reason with how many, without their addresses.
  Without the kernel's `nft_log` and `nfnetlink_log` modules, fence 2 only
  counts; when it cannot count either, it records nothing, and a `gap`
  record says so. `brig audit status` and the Host page of brig ui show
  which, and `brig doctor` checks it.

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

- Which sandbox or program sent a packet that fence 2 dropped: that would
  need code inside the VM. The trail names the likely sandbox only.
- Packets that fence 2 lets through; fence 1's requests cover them.
- The trail lives in your home directory and is not tamper-evident: it guards
  against what happens in the VM, not against your own user account.

[Look back at what a sandbox did](../how-to/audit.md) shows the commands that
read it.
