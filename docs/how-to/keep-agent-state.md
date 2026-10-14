# Keep agent state

A sandbox keeps its work in `/sandbox`: checkouts, agent settings and
sessions. Most events keep it, and brig keeps it when it moves a sandbox to a
new image. Only deletes lose it: `openshell sandbox delete`, `brig delete`, and
a `--no-keep` sandbox when its main process exits; never use `--no-keep` for
interactive work. Snapshots cover the deletes you choose and anything risky,
and git covers the code.

| Event | Running agent | `/sandbox`: checkouts, agent settings and sessions | Gateway: providers, profiles, templates | Images in the VM |
| --- | --- | --- | --- | --- |
| `Ctrl-P`, `Ctrl-Q`; laptop sleep; network drop | keeps running | kept | kept | kept |
| The agent exits or crashes | gone; `--continue` resumes | kept, sessions are written as you go | kept | kept |
| `openshell sandbox stop` / `start` | gone | kept | kept | kept |
| `brig stop` / `brig start` | gone | kept | kept, then re-synced | kept |
| `brig agent update` / `brig sandbox recreate` | gone | **kept**: restored into the new sandbox, with a snapshot left behind | kept; providers attached again, the policy history starts over, the audit trail keeps the old one | kept |
| `brig upgrade` | gone | kept: data disk; across an OpenShell minor version, every sandbox is snapshotted first | kept: data disk | kept |
| `openshell sandbox delete` | gone | **lost**; `brig sandbox delete --snapshot` keeps a snapshot first | kept | kept |
| `brig delete` | gone | **lost**, snapshots included: they live on the VM's data disk | **lost**; config dirs rebuild profiles and providers | **lost** |

## Habits

1. **Git is the record for code.** Grant the sandbox push access to its
   repository, on [GitHub](git-access.md#grant-push-and-pull-requests-for-one-repository)
   or [GitLab](git-access.md#grant-push-and-merge-requests-for-one-project), and have the
   agent commit and push a work branch often. Snapshots live in the VM and go
   with it, so they don't replace pushes.
2. **Move to a new image with `brig agent update`**, not by deleting and
   creating. After `brig agent build dev pi`, it recreates each of the agent's
   sandboxes on the new image and restores its `/sandbox`, sessions included:

   ```sh
   brig agent update dev pi --dry-run
   brig agent update dev pi
   ```

   `brig sandbox recreate dev webapp --image REF` does the same for one
   sandbox and any image. See [Upgrade](upgrade.md#sandbox-image).
3. **Snapshot before anything risky**, such as letting an agent reorganise a
   checkout or trying a new image, and restore to go back. See
   [Snapshots](#snapshots).
4. **Prune now and then.** A snapshot costs little when it is taken but grows
   as the sandbox moves on. `brig sandbox snapshot list dev` shows what each
   holds alone, and `brig sandbox snapshot prune dev` deletes the snapshots of
   deleted sandboxes and keeps the newest of each other sandbox.
5. **Keep what you would miss in `/sandbox`.** `/tmp` and the rest of the
   container layer may not survive a restart, and a snapshot holds only
   `/sandbox`.
6. **Rebuild the VM from files, not memory.** Profiles, providers and the
   default policy come back from your config directories on the first `brig
   start`; recreate templates with a short script kept next to them.

## Snapshots

A snapshot is a copy of one sandbox's `/sandbox`, named `SANDBOX@TIME` with
the time in UTC. brig stops a ready sandbox for the copy and starts it again,
and asks first if an agent runs in it:

```sh
brig sandbox snapshot create dev webapp
brig sandbox snapshot list dev
```

Restore replaces the sandbox's `/sandbox` with the snapshot; what is there now
is lost, so take a snapshot first if you may want it back:

```sh
brig sandbox snapshot restore dev webapp@2026-10-14T15:02:11Z webapp
```

Restore into another sandbox to fork the work: create a sandbox from the same
template, then restore into it, and try something there while `webapp` goes
on:

```sh
brig sandbox create dev -- --name webapp-try --template pi --provider llm
brig sandbox snapshot restore dev webapp@2026-10-14T15:02:11Z webapp-try
```

`brig sandbox delete --snapshot dev webapp` keeps a snapshot before it
deletes, and the snapshot outlives the sandbox. `brig sandbox snapshot prune
dev --keep 3 --older-than 30d` keeps the newest three of each sandbox and
deletes any older than 30 days; `--dry-run` shows what it would delete.

brig warns when a snapshot holds a real credential, such as the file that
`/login` inside a sandbox leaves. Log in through providers instead, as the
[agents](agents.md) guide says.

### By hand

For an image that brig does not know, `openshell sandbox download` and
`upload` copy files out of a sandbox and into a new one. Paths are relative to
`/sandbox`; Pi keeps its sessions under `.pi/agent/sessions`, Claude Code under
`.claude/projects`:

```sh
openshell sandbox download webapp .pi/agent/sessions ~/pi-sessions/webapp/sessions
openshell sandbox upload webapp ~/pi-sessions/webapp/sessions .pi/agent
```

## Data disk

Everything kept here lives on the VM's [data
disk](../concepts/disks-and-images.md#disks), and so do snapshots, which fill it
up over time along with the workspaces and images: `df -h /home/agent` in `brig
ssh dev` shows how full it is, `brig sandbox snapshot list dev` what the
snapshots hold alone, and `brig update dev --data-disk 80GiB` grows it at the
next boot.
