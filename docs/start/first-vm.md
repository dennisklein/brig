# Your first VM

This creates a VM named `dev`, the name the other guides use, and opens a
first sandbox on it. It assumes brig is [installed](install.md) with its shell
integration.

```sh
brig create dev                 # builds a base image first if needed
brig list
brig use dev                    # point this shell's openshell CLI at the VM's gateway
openshell sandbox create        # a shell in OpenShell's default image
brig ssh dev                    # a shell in the VM, as the gateway's user
```

`brig create` stops before it changes anything if a `brig doctor` check fails.
The first run builds a [base image](../concepts/disks-and-images.md#base-images)
on your laptop, which takes a while; later VMs reuse it.

Without the shell integration, `brig use dev` fails: run `eval "$(brig env
dev)"` instead, or set up the [integration](install.md#shell-integration).

## What you now have

- **A VM** `dev`, running rootless in your libvirt user session. virt-manager
  shows it as `brig-dev`, but start it with `brig start`, which also starts its
  network.
- **A gateway**: OpenShell's control plane inside the VM, which your
  `openshell` CLI reaches as `brig-dev`. `brig use dev` points the current
  shell at it.
- **A sandbox**: the container that `openshell sandbox create` started, with
  its own policy. Leaving the shell leaves it running; `openshell sandbox list`
  shows it.

[Architecture](../concepts/architecture.md) explains how the pieces fit, and
which of the two fences each request passes.

## Stop, start and delete

```sh
brig stop dev                   # shut down; everything on the data disk stays
brig start dev                  # boot, re-register the gateway and sync
brig up dev                     # start if needed, wait for the gateway, list sandboxes
```

`brig delete dev` deletes the VM with its data disk, every sandbox and every
secret its gateway holds. It asks first on a terminal. Keep `dev` for now: the
next guide builds on it.

## Next

- [Run your first agent](first-agent.md) in a sandbox.
- [Share a host directory](../how-to/share-directories.md) with the VM.
- Change CPUs, memory and disks with `brig update dev`; see `brig update
  --help`.
