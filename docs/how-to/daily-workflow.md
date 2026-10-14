# Daily workflow

Keep one VM, and one long-lived sandbox per project.

## Start of day

1. `brig up dev` boots the VM, re-registers the gateway, syncs your config
   directories and lists the sandboxes, with the drafted rules that wait for
   your review; the keyring may ask to be unlocked. It fails if the gateway
   does not answer or the sync fails, where `brig start dev` only warns.
2. `brig use dev` in each terminal that runs `openshell`.
3. Start the sandboxes that are `Stopped` or `Completed` with `brig sandbox
   start dev webapp`.
4. `brig sandbox connect dev webapp`, then in the sandbox `cd webapp && git
   pull` and `pi-sandbox --continue` to resume the last conversation for that
   directory.

## During the day

- Keep a second terminal on `openshell term`, or `brig sandbox logs dev
  webapp -f`, to see what the agent tries and, for each denial, the method,
  path and rule that refused it.
- Grant missing access for this sandbox only, and keep it in the project's
  policy file if it should last: `brig policy allow dev webapp HOST --binary
  PATH --save`. [Unblock an agent](unblock.md) has the details.
- Review on the host: `git fetch` the agent's branch and read the diff before
  anything merges.
- Rotated a token? `brig secret set dev github` stores it again and updates
  that provider; see [secrets](secrets.md#rotate-a-token).
- Rebuilt an agent's image with `brig agent build dev pi`? `brig agent update
  dev pi` moves its sandboxes to it, keeping `/sandbox`; see
  [Upgrade](upgrade.md#sandbox-image).

## End of day

1. Have the agent push its work branch.
2. Leave everything running, or free memory with `brig sandbox stop dev
   webapp` and `brig stop dev`. Both keep `/sandbox`.

## New and finished projects

```sh
brig sandbox create dev --connect -- --name tools --template pi \
  --provider llm --provider gitlab-work \
  --policy ~/src/agent-openshell/policies/tools.yaml
```

`--connect` starts the VM if it is stopped, waits until the sandbox is ready
and connects to it. Attach only the providers a project needs: a provider's
secret is reachable only from sandboxes it is attached to. When a project is
done, push, then `brig sandbox delete --snapshot dev tools`, which keeps a
[snapshot](keep-agent-state.md#snapshots) of its `/sandbox` until you prune
it.

For a task that should run in a fresh sandbox and leave nothing behind, see
[one-shot runs](one-shot-runs.md).
