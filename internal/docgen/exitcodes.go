// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dennisklein/brig/internal/cli"
)

// exitCodesPage lists the exit statuses from the table that brig --help and
// the JSON errors use too.
func exitCodesPage() string {
	var b strings.Builder
	b.WriteString(generatedMark)
	b.WriteString(`
# Exit codes

A script can tell why brig failed from its exit status. ` + "`brig --help`" + ` lists the
same statuses.

| Status | Reason | Meaning |
| --- | --- | --- |
`)
	codes := cli.ExitCodes()
	slices.SortFunc(codes, func(a, b cli.ExitCode) int { return a.Code - b.Code })
	for _, c := range codes {
		reason := ""
		if c.Reason != "" {
			reason = code(c.Reason)
		}
		fmt.Fprintf(&b, "| %d | %s | %s |\n", c.Code, reason, cell(c.Meaning))
	}
	b.WriteString(`
## Errors as JSON

A command run with ` + "`-o json`" + ` that printed nothing on standard output yet
fails also prints the error there, as a JSON object. Its ` + "`reason`" + ` is the
reason of the exit status in the table above:

` + fence("json", "{", `  "error": "no such VM: nosuch",`, `  "reason": "not_found",`, `  "exit_code": 3`, "}") + `
## Statuses of particular commands

These commands use statuses of their own, or pass one on. Their pages say more.

| Command | Status |
| --- | --- |
| [` + "`brig policy diff`" + `](cli/brig-policy-diff.md) | 0 when the sandbox agrees with its policy file, 1 when it differs, 7 when brig cannot tell which file to compare with. |
| [` + "`brig sandbox run`" + `](cli/brig-sandbox-run.md) | The status of the command in the sandbox, 124 if ` + "`--timeout`" + ` stopped it, 130 after Ctrl-C and 143 after SIGTERM. brig's own failures use the table above. |
| [` + "`brig sandbox exec`" + `](cli/brig-sandbox-exec.md) | The status of the command in the sandbox. brig's own failures, before the command starts, use the table above. |
| [` + "`brig audit status`" + `](cli/brig-audit-status.md) | 1 when a collector failed or does not run while its VM does, so that the VM's traffic is not recorded. |
| [` + "`brig up`" + `](cli/brig-up.md) | 3 if the VM does not exist and ` + "`--create`" + ` is not given, 5 if its gateway does not answer, 6 if the config sync fails. |
| [` + "`brig start --wait`" + `](cli/brig-start.md) | 5 if the gateway does not answer, 6 if the config sync fails. Without ` + "`--wait`" + ` they only warn. |
`)
	return b.String()
}
