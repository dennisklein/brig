# Docs

brig's documentation is plain Markdown in `docs/`, read on GitHub. Pages are
grouped by what the reader wants:

| Directory | Holds | Example |
| --- | --- | --- |
| `start/` | tutorials, run top to bottom | [install](../start/install.md) |
| `how-to/` | one task per page | [unblock an agent](../how-to/unblock.md) |
| `concepts/` | explanation: how brig works and why | [architecture](../concepts/architecture.md) |
| `reference/` | what the code knows, generated from it | [exit codes](../reference/exit-codes.md) |
| `contributing/` | building, releasing and packaging brig | this page |

`README.md` stays short: what brig is, install, a quick start and links here.
`docs/index.md` maps the pages by question.

## One fact, one home

Anything the code knows, such as commands, flags, config keys, defaults, exit
codes, network profiles and brig ui's pages and actions, belongs in
`reference/`, which is generated from the code. Hand-written pages link to it
instead of restating it.

The help texts in `internal/help` are the short form of the concepts and
how-tos; the pages here are the long form. A topic's `doc:` field names the
page and heading that `brig help TOPIC` and brig ui's help pane link to, as a
path under `docs/`, such as `concepts/architecture.md#the-two-fences`. A test
checks that each `doc:` names a file and heading that exist.

`docs/how-to/agents.md` shows the files that `brig agent init` writes. A code
block after an `<!-- agent-file: KIND PATH -->` marker must match that file
as rendered for the page's examples; `internal/agent`'s tests check it.

## Style

- Short, plain sentences, in the words the CLI and brig ui use: VM, gateway,
  sandbox, workspace, provider, provider profile, policy, fence 1 and fence 2.
- Examples use the VM `dev`, the sandbox `webapp` for the repository
  `alice/webapp`, and the config directory `~/src/agent-openshell`.
- Commands in `sh` blocks are meant to be pasted; comments say what a line
  does, not what to type.
- Plain GitHub Markdown, without site-only syntax, so the repository is the
  documentation. Mermaid diagrams are fine: GitHub renders them.
- Link OpenShell's files and docs at the version brig targets, such as
  `v0.1.4`, and update them with the version.
