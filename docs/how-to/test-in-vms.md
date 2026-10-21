# Test in throwaway VMs

An agent that must try its work on a whole OS, such as an installer, a
desktop application or a system service, needs a VM of its own. The vm
runner service gives agents in sandboxes such VMs on your host, from images
that you allow, and MCP tools to drive them: screenshots, clicks and typing,
commands and file copies through the guest agent, and snapshots. brig calls
these VMs spike VMs.

## Allow images

The vm runner starts VMs only from the images that `vm_runner.images` in
`~/.config/brig/config.yaml` names. Each is a qcow2 file that contains
`qemu-guest-agent`, started at boot; the Fedora and Ubuntu cloud images do.
brig never writes to the file: each VM gets an overlay disk on top of it.

```yaml
vm_runner:
  images:
    fedora-44:
      file: ~/vm-images/Fedora-Cloud-Base-Generic-44.x86_64.qcow2
```

The image must be one file, without a backing file. An agent picks an image
by its name, such as `fedora-44`.

## Create the service and attach it

```sh
brig service create vms --type vm
brig service attach vms dev webapp
```

The service is a controller on the host, the user unit
`brig-vm-runner-vms.service`. It serves MCP on a port of the host's loopback
interface, which the gateway reaches for the attached sandboxes only (see
[services](../concepts/services.md)). Programs in sandbox `webapp` see the
token's placeholder in `$VMS_TOKEN`, which `brig service show vms` lists;
OpenShell swaps it for the real token, and the controller knows from it
which sandbox calls.

Add the server to the agent, for example to Claude Code in the sandbox:

```sh
claude mcp add --transport http vms http://host.openshell.internal:PORT/ \
  --header "Authorization: Bearer $VMS_TOKEN"
```

`PORT` is the service's port that `brig service show vms` shows.

## What agents can do

| Tool | What it does |
|------|--------------|
| `vm_images` | Lists the images. |
| `vm_create` | Creates and starts a VM and waits for its guest agent. |
| `vm_list`, `vm_start` | Lists the sandbox's VMs; boots one that is shut off. |
| `vm_screenshot` | Returns the screen as a PNG image. |
| `vm_click`, `vm_type`, `vm_key` | Moves the pointer and clicks, types text, presses key combinations. |
| `vm_exec` | Runs a program as root in the VM and returns its exit code and output. |
| `vm_copy_in`, `vm_copy_out` | Writes and reads files in the VM, 16 MiB per call. |
| `vm_snapshot`, `vm_restore`, `vm_snapshot_delete` | Saves, restores and deletes the VM's disk and memory. |
| `vm_destroy` | Deletes the VM. |

A sandbox sees and drives only the VMs that it started. OpenShell's policy
can allow a sandbox some tools and deny others.

## Limits

Each VM has no network card: it reaches neither the internet nor your LAN
nor the host. Files go in and out only through `vm_copy_in` and
`vm_copy_out`. It has no shared directories and no nested virtualisation.

`vm_runner.quota` bounds what each sandbox may use at once, and
`vm_runner.host_quota` what all sandboxes may use together. The built-in
values are in the [configuration reference](../reference/config.md). brig
destroys a VM when its lifetime ends, and when the sandbox that started it
loses its token, as when you detach the service or delete the sandbox.

## Watch and clean up

```sh
brig vm-runner list
brig vm-runner screenshot 3f9c1a2e
brig vm-runner destroy 3f9c1a2e
```

`brig vm-runner list` shows every VM with the sandbox that started it, and
`brig vm-runner screenshot` saves a VM's screen as `ID.png`. `brig vm-runner
destroy` deletes VMs of any sandbox.

`brig service delete vms` destroys all VMs of the service before it stops
the controller.
