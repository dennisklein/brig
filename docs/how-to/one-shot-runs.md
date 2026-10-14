# One-shot runs

A harness that wants a task run in a fresh sandbox, with files in and out,
uses `brig sandbox run`. It creates the sandbox, uploads, runs the command,
downloads and deletes the sandbox again, also on Ctrl-C:

```sh
brig sandbox run dev --template pi --provider llm --timeout 30m \
  --upload ./task --download task/out --result run.json -- make -C task
```

The command's output and exit status are brig's, 124 after `--timeout`;
brig's own failures exit with the statuses that `brig --help` and [exit
codes](../reference/exit-codes.md) list.
`run.json` records the sandbox, the exit status, the transfers and the
requests that the policy denied, by host and binary. `--keep failed` keeps the
sandbox of a failed run to look into; `brig sandbox clean dev --runs` deletes
the kept and left-over sandboxes of runs once they expire. `--dry-run` prints
the `openshell` commands that a run amounts to.
