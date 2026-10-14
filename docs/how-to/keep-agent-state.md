# Keep agent state

`openshell sandbox delete` loses a sandbox's work, and you run it to move a
sandbox to a new image. `brig delete` loses it too, and so does a `--no-keep`
sandbox when its main process exits; never use `--no-keep` for interactive
work. Everything else, from detaching to `brig upgrade`, keeps `/sandbox`. So
treat a sandbox as long-lived, push code continuously, and copy the agent's
sessions out before you delete.

| Event | Running agent | `/sandbox`: checkouts, agent settings and sessions | Gateway: providers, profiles, templates | Images in the VM |
| --- | --- | --- | --- | --- |
| `Ctrl-P`, `Ctrl-Q`; laptop sleep; network drop | keeps running | kept | kept | kept |
| The agent exits or crashes | gone; `--continue` resumes | kept, sessions are written as you go | kept | kept |
| `openshell sandbox stop` / `start` | gone | kept | kept | kept |
| `brig stop` / `brig start` | gone | kept | kept, then re-synced | kept |
| `brig upgrade` | gone | kept: data disk | kept: data disk | kept |
| `openshell sandbox delete`, or recreate for a new image | gone | **lost** | kept | kept |
| `brig delete` | gone | **lost** | **lost**; config dirs rebuild profiles and providers | **lost** |

## Habits that make deletes safe

1. **Git is the record for code.** Grant the sandbox push access to its
   repository, on [GitHub](git-access.md#grant-push-and-pull-requests-for-one-repository)
   or [GitLab](git-access.md#grant-push-and-merge-requests-for-one-project), and have the
   agent commit and push a work branch often.
2. **Copy sessions out before a delete.** Pi's sessions are JSONL files under
   `/sandbox/.pi/agent/sessions`, grouped by working directory; `download`
   paths are relative to `/sandbox`:

   ```sh
   openshell sandbox download webapp .pi/agent/sessions ~/pi-sessions/webapp/sessions
   ```

   After recreating the sandbox and cloning to the same path, upload them so
   `pi-sandbox --continue` finds them:

   ```sh
   openshell sandbox upload webapp ~/pi-sessions/webapp/sessions .pi/agent
   ```

   For a single conversation, Pi's `/export` writes HTML or JSONL instead.
   Claude Code keeps its sessions under `/sandbox/.claude/projects`; copy
   that directory out and back the same way.
3. **Keep what you would miss in `/sandbox`.** `/tmp` and the rest of the
   container layer may not survive a restart.
4. **Rebuild the VM from files, not memory.** Profiles, providers and the
   default policy come back from your config directories on the first `brig
   start`; recreate templates with a short script kept next to them.

Everything kept here lives on the VM's [data
disk](../concepts/disks-and-images.md#disks), which fills up over time: `df -h
/home/agent` in `brig ssh dev` shows how full it is, and `brig update dev
--data-disk 80GiB` grows it at the next boot.
