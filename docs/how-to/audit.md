# Look back at what a sandbox did

brig keeps an [audit trail](../concepts/audit-trail.md) of each VM: every
request a sandbox's proxy allowed or denied, each policy revision with who
made it, drafts, sandboxes and brig commands. These commands read and manage
it. `brig help look-back` has the short version.

## Read the trail

```sh
brig audit dev webapp --since 7d        # a timeline, oldest first
brig audit dev webapp --summary         # each endpoint reached, with the rule that allowed it
brig audit dev webapp --denied -f       # follow denials as they happen
brig audit dev --kind policy,draft      # only policy changes and drafts
brig audit dev -o json | jq …           # the records as stored, one per line
```

`--since` and `--until` take a duration back from now (`30m`, `24h`, `7d`),
a date or a time; the default is the last day, and `--since all` reads
everything. `--host` filters by host. A deleted sandbox's history stays, so
its name still works. Denials do not change the exit status.

In brig ui, Traffic shows the same trail: pick 24 hours, 7 days or all
instead of the last hour. A sandbox's History tab lists its policy revisions
with who made each, its drafts and lifecycle, and the endpoints it reached in
the last week. See [the web UI](web-ui.md#traffic-and-history).

## Check the collectors

```sh
brig audit status        # whether each VM's collector runs, gaps, space used
brig audit status dev    # the same, broken down by sandbox, kind and month
```

A running VM whose traffic nothing records shows under Needs you in brig ui.

## Free space

Nothing deletes history unless you ask:

```sh
brig audit prune dev --before 90d --dry-run   # say what would go and what it frees
brig audit prune dev --before 90d
brig audit prune dev --sandbox old-project
brig audit prune --deleted-vms                # the history of VMs that brig delete deleted
```

On a terminal, prune asks first. To prune automatically, set
`audit.retention`, such as `90d`, in [config.yaml](../reference/config.md).
