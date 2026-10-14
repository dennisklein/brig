# brig documentation

brig runs coding agents in OpenShell sandboxes inside VMs on your own
machine. These pages are grouped by what you want: to get started, to get
something done, to understand how it works, or to look something up.

## Start

New to brig? Run these in order.

1. [Install brig](start/install.md), its host dependencies and the shell
   integration.
2. [Create your first VM](start/first-vm.md) and open a sandbox on it.
3. [Run your first agent](start/first-agent.md): Claude Code, Pi or opencode.

## How to

- How do I start the day, work with an agent, and finish? [Daily
  workflow](how-to/daily-workflow.md)
- An agent cannot reach a host. [Unblock an agent](how-to/unblock.md)
- How do I let an agent push and open pull requests? [Git
  access](how-to/git-access.md)
- How do I change an agent's image, model or instructions?
  [Agents](how-to/agents.md)
- How do I give a sandbox a host directory? [Share
  directories](how-to/share-directories.md)
- How do I store or rotate a token, or run in CI? [Secrets](how-to/secrets.md)
- How do I keep an agent's work when I move it to a new image or delete it?
  [Keep agent state](how-to/keep-agent-state.md)
- How do I run one task in a throwaway sandbox? [One-shot
  runs](how-to/one-shot-runs.md)
- How do I move to a newer OpenShell or Fedora? [Upgrade](how-to/upgrade.md)
- What did a sandbox reach, and who allowed it? [Look
  back](how-to/audit.md)
- How do I use brig from scripts? [Scripting](how-to/scripting.md)
- What does `brig ui` show and do? [The web UI](how-to/web-ui.md)
- Something fails. [Troubleshooting](troubleshooting.md)

## Concepts

- [Architecture](concepts/architecture.md): host, VM, gateway and sandbox,
  the two fences, and where secrets go.
- [OpenShell configuration](concepts/openshell-config.md): providers,
  profiles, policies, and the config directories brig syncs.
- [Workspaces](concepts/workspaces.md): tenants inside a gateway.
- [Disks and images](concepts/disks-and-images.md): what a VM boots from and
  what survives an upgrade.
- [Audit trail](concepts/audit-trail.md): what brig records about each
  sandbox, and its gaps.

## Reference

- [Commands](reference/cli/README.md), one page per command; `brig COMMAND
  --help` in a terminal.
- [Configuration](reference/config.md): `~/.config/brig/config.yaml`.
- [Network profiles](reference/network-profiles.md): the built-in profiles.
- [Exit codes](reference/exit-codes.md).
- [Glossary](reference/glossary.md): every concept, as `brig help TOPIC`
  explains it.
- [Web UI](reference/ui.md): every page and action of `brig ui`.

## Contributing

- [Development](contributing/development.md): tools, tests and lint.
- [Releasing](contributing/releasing.md).
- [Packaging](contributing/packaging.md): the package repository and its
  workflows.
- [Docs](contributing/docs.md): how these pages are laid out and written.
