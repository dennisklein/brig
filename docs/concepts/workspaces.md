# Workspaces

A workspace is a tenant inside a gateway, with its own sandboxes, providers
and provider profiles. Every gateway has the workspace `default`, and most
setups need no other.

## Picking a workspace

`-w WORKSPACE` picks one on every command that works in one: the `brig
sandbox`, `policy`, `secret` and `audit` commands and `brig agent build`.
Without `-w` they work in the workspace that `BRIG_WORKSPACE` names, else in
`default`. `brig sync`, where `-w` is repeatable, falls back to every
workspace the config directories define instead, and `brig sandbox list`
lists every workspace unless `-w` names one.

`brig use dev -w team` exports `BRIG_WORKSPACE=team`, and `OPENSHELL_WORKSPACE=team`, which makes
plain `openshell` commands work in the same workspace. `brig use dev` clears
it, and `brig use -w team` changes the workspace and keeps the VM.

brig ui shows the sandboxes and providers of every workspace.

## Config directories

`profiles/` and `providers/` at the top of a [config
directory](openshell-config.md#config-directories) are for the gateway's
`default` workspace. For another workspace, put the same directories under
`workspaces/NAME/`:

```text
~/src/agent-openshell/
  profiles/                 # workspace default
  workspaces/team/
    profiles/
    providers/
  policies/                 # not per workspace
```

brig creates a missing workspace, never deletes one, and syncs each workspace
on its own (`brig sync dev --workspace NAME` limits a sync). The same id or
name may appear in several workspaces, `--prune` only deletes inside the
workspace where brig created an item, and `policies/` is not per workspace.
