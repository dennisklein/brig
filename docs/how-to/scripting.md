# Scripting

brig is meant to be scripted: its results go to standard output, everything
else to standard error, `-o json` gives machine-readable output, and the exit
status says why a command failed.

## Standard output and standard error

Standard output carries only a command's result: a table, JSON, or one line
such as `Created VM dev from image …`. Progress, notes, hints and warnings go
to standard error. Where a command takes `[NAME]` and you leave it out, it
works on the VM that `brig use` selected and says which on standard error.

## JSON output

`-o json` works on the commands that read and on those that change a VM:
`create`, `start`, `stop`, `update` and `upgrade` print the VM as `show -o
json` does once they are done, `sync -o json` prints each workspace, profile
and provider with what it did to it (or would, with `--dry-run`) and the
error if the sync failed, and `image push -o json` prints the VM and the
image. `brig COMMAND --help` says whether a command has `-o`.

With `-o json`, a command that fails before it printed anything prints the
error on standard output as well, as `{"error": "...", "reason": "not_ready",
"exit_code": 5}`.

## Exit status

brig's exit status says why a command failed: not found, exists already, not
ready, or a failed sync, each with its own status and JSON `reason`. [Exit
codes](../reference/exit-codes.md) lists them, as does `brig --help`. `brig
policy diff` uses 1 and 7 for its outcomes (see its `--help`), and `brig
sandbox exec` and `brig sandbox run` exit with the command's status.

## Progress

Commands that take a while, such as `create`, `start`, `stop`, `upgrade`,
`delete` and `image build`, show what they are doing on standard error.
Steps that the command goes on without, such as a gateway that does not
answer at `brig start`, end as skipped with the reason. `ssh`, `console`,
`env`, `use`, `shell-init` and `sandbox create` show no progress, since they
own the terminal or must print exactly their output, and neither does `ui`.

`--progress MODE` (or `BRIG_PROGRESS`) picks the display: `auto` (the
default) chooses from where standard error goes, `tty` redraws a tree of
steps, `counter` a single line, `plain` prints one line per event, and `none`
shows nothing. On a terminal, `NO_COLOR` turns colours off. Text from guests,
images and libvirt is escaped before it is shown, so it cannot inject terminal
control sequences.

## The activity log

Commands that show progress also append a JSON event log of their work to
`$XDG_STATE_HOME/brig/activity.jsonl` (usually
`~/.local/state/brig/activity.jsonl`), which brig moves to
`activity.jsonl.1` once it passes 8 MiB. `--progress-log FILE` (or
`BRIG_PROGRESS_LOG`) logs to FILE instead; an empty value turns the log off.
A caller can pass a W3C trace context in `TRACEPARENT` and `TRACESTATE` to
find a run in the log by its trace id. brig ui's Activity page reads the same
log.
