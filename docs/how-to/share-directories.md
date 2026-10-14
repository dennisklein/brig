# Share directories

A host directory can be shared into a VM, and from there offered to its
sandboxes. **Sandboxes only ever see it read-only**, so an agent cannot change
files on your host. To let an agent work on a project, have it clone the
repository into `/sandbox` and push a branch, as in [Git
access](git-access.md); mounts are for what the agent reads, such as plugins
you are developing or reference data.

## Into a sandbox

`brig sandbox create` does everything in one step: it gives the VM a sandbox
mount of each directory it lacks one of, and runs `openshell sandbox create`
on the VM's gateway with the volumes attached, passing on what follows `--`:

```sh
brig sandbox create dev --mount ~/src/pi-kit:/usr/local/share/pi-kit -- --name kit
```

Edits on the host show in the sandbox at once. The VM keeps the mount for
later sandboxes; `brig show dev` lists it with its volume, and `brig update
dev --remove-mount` removes it once no sandbox uses it. [Developing plugins
without rebuilding](agents.md#developing-plugins-without-rebuilding) shows it
at work. Mount under `/usr` to keep the directory readable under the default
policy.

In brig ui, open the VM, then Mounts, press Add mount and tick Offer to
sandboxes; then pick the directory under Host directories when you create the
sandbox.

## Into the VM

`--mount SOURCE[:TARGET][:OPTIONS]` on `brig create` shares a host directory
with the VM, where OPTIONS is a comma-separated list of `ro` (the default),
`rw` and `sandbox`. `brig update --add-mount` and `--remove-mount` change the
mounts of a running VM at once. Mounts need virtiofsd on the host.

In the VM, user `agent` owns what you own there, and what it writes is yours;
the VM's other users map to your subordinate IDs, which need entries in
`/etc/subuid` and `/etc/subgid` (Fedora adds them for new users).

With `sandbox`, which requires `ro`, OpenShell sandboxes can attach the
directory read-only as a Podman volume named after the mount's tag, which
`brig show` lists (e.g. `brig0`). That is what `brig sandbox create --mount`
sets up for you.
