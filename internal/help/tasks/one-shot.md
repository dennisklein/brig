---
title: "Run a task in a throwaway sandbox"
concepts: [sandboxes, provider, policies]
commands:
  - "brig sandbox run dev --template pi --provider llm --timeout 30m --upload ./task --download task/out --result run.json -- make -C task"
  - "brig sandbox clean dev --runs"
doc: "how-to/one-shot-runs.md"
---
1. brig sandbox run creates a sandbox, uploads files, runs the command,
   downloads results and deletes the sandbox, also on Ctrl-C.
2. Its output and exit status are the command's. --result writes a record of
   the run with the requests that the policy denied.
3. --keep failed keeps a failed run's sandbox to look into; brig sandbox
   clean --runs deletes kept ones once they expire.
