# Your first agent

This puts a coding agent into a sandbox on the VM `dev` from [your first
VM](first-vm.md): Claude Code on your Claude subscription, or Pi or opencode
on a model endpoint of your choice. The agent gets its credentials as
placeholders; the real tokens stay in your keyring and the VM's gateway.

## Write and apply the agent's files

`brig agent init` writes everything an agent needs into [config
directories](../concepts/openshell-config.md#config-directories): the image
definition, the provider profile and provider, and a default policy. With
`--vm` it also applies them and builds the image. Pick one:

```sh
brig agent init claude --dir ~/src/agent-openshell --vm dev
brig agent init pi --dir ~/src/agent-openshell --vm dev \
  --endpoint https://llm.example.com/v1 --model your-model-id
brig agent init opencode --dir ~/src/agent-openshell --vm dev --anthropic
```

For Claude Code, run `claude setup-token` on the host first: it signs you in
through the browser and prints the token that brig asks for.

With `--vm`, brig names each step as it goes: it attaches the directories to
the VM, asks for each missing secret (on a terminal), syncs, builds the image,
and prints the command that starts a sandbox. Without `--vm` it only writes
files and prints the commands to run, so you can review the files first,
which matters when the directory is a shared git repository.

| | Claude Code | Pi | opencode |
| --- | --- | --- | --- |
| Model access | subscription token | `--endpoint` and `--model`, provider `llm` | the same, or `--anthropic` with an API key |
| Written | `agents/NAME/`, `profiles/claude-code.yaml` | `agents/NAME/`, `profiles/llm.yaml` | `agents/NAME/`, `profiles/llm.yaml` or `anthropic.yaml` |

`NAME` defaults to the kind. `--github LOGIN` also writes a `github`
provider. Provider files go to `~/.config/brig/openshell/providers/`, or
wherever `--secrets-dir` says, and hold no secrets. brig never overwrites a
file: one with the same content is skipped, one with other content stops the
command with a list, unless `--force`. A profile, provider or default policy
that another directory of the VM defines already is left out. The
Containerfiles pin the agent versions, which move with brig releases.

`agents/NAME/agent.yaml` says what belongs together:

```yaml
image: localhost/claude-agent     # brig agent build adds a date tag
template: claude                  # kept pointing at the newest build
providers: [claude-code, github]  # what a new sandbox of this agent attaches
command: claude-sandbox           # shown by Connect; informational
```

## Start a sandbox

Run the command that `brig agent init` printed, such as:

```sh
brig sandbox create dev --connect -- --name webapp --template claude \
  --provider claude-code --provider github
# now in a login shell inside the sandbox
git clone https://github.com/alice/webapp.git && cd webapp
claude-sandbox                    # or pi-sandbox, opencode-sandbox
```

`--connect` starts the VM if needed, waits until the sandbox is ready and
connects to it. What follows `--` goes to `openshell sandbox create`. Press
`Ctrl-P`, `Ctrl-Q` to detach with the agent still working; `brig sandbox
connect dev webapp` reattaches.

The sandbox can read what its providers allow, such as clones from GitHub,
but cannot push yet. [Git access](../how-to/git-access.md) grants pushes for
one repository.

## Next

- [Agents](../how-to/agents.md) explains the files `brig agent init` wrote and
  how to change them; run `brig agent build dev NAME` after each change.
- [Daily workflow](../how-to/daily-workflow.md): start of day, during, end.
- [Unblock an agent](../how-to/unblock.md) when a request is denied.
- [Keep agent state](../how-to/keep-agent-state.md) before you delete a
  sandbox.
