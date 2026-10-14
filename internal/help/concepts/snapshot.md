---
title: "Snapshot"
summary: "A copy of one sandbox's /sandbox, kept in the VM on the data disk. It outlives the sandbox and goes with the VM."
aliases: ["backup", "restore", "fork"]
see: [sandboxes, recreate, data-disk]
commands:
  - "brig sandbox snapshot create dev webapp"
  - "brig sandbox snapshot list dev"
  - "brig sandbox snapshot restore dev webapp@2026-10-14T15:02:11Z webapp"
  - "brig sandbox snapshot prune dev"
doc: "how-to/keep-agent-state.md#snapshots"
---
A snapshot is named SANDBOX@TIME, the time in UTC. It shares its blocks with
the sandbox it came from, so it takes seconds and almost no space at first and
grows as the sandbox moves on; snapshot list shows what each holds alone.

Restore replaces a sandbox's /sandbox with a snapshot, also another sandbox's
to fork the work. Recreate and sandbox delete --snapshot take one first, and
nothing deletes a snapshot but delete, prune and brig delete.
