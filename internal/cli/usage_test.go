// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/spf13/cobra"
)

// A command line that is wrong ends with status 2, so that a script tells
// it from a command that ran and failed.
func TestUsageErrorsExitWith2(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"frobnicate"}, `unknown command "frobnicate" for "brig"`},
		{[]string{"sandbox", "nosuch"}, `unknown command "nosuch" for "brig sandbox"`},
		{[]string{"sandbox", "crate"}, `did you mean "create"?`},
		{[]string{"help", "nosuch"}, `unknown help topic "nosuch"`},
		{[]string{"list", "--nosuch"}, "unknown flag: --nosuch"},
		{[]string{"list", "extra"}, "(usage: brig list"},
		{[]string{"show", "-o", "yaml"}, "unknown output format"},
	} {
		out, err := run(t, tc.args...)
		if code := exitStatus(err); code != exitUsage || err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("brig %q: exit status %d, %v (%s); want %d and %q", tc.args, code, err, out, exitUsage, tc.message)
		}
		if err != nil && !cobratree.IsUsage(err) {
			t.Errorf("brig %q: %v is no usage error", tc.args, err)
		}
	}
}

// A command that only holds subcommands prints its help, and succeeds, when
// it is given no word.
func TestGroupWithoutAWordPrintsItsHelp(t *testing.T) {
	for _, group := range []string{"sandbox", "policy", "image"} {
		out, err := run(t, group)
		if err != nil || !strings.Contains(out, "brig "+group) || !strings.Contains(out, "Available Commands") {
			t.Errorf("brig %s: %v\n%s", group, err, out)
		}
	}
	if out, err := run(t, "help", "sandbox"); err != nil || !strings.Contains(out, "Available Commands") {
		t.Errorf("brig help sandbox: %v\n%s", err, out)
	}
}

// brig audit and brig net-helper hold subcommands and run on their own.
func TestGroupsThatRunAreNotGroups(t *testing.T) {
	root := newRootCmd(&progressRun{})
	for _, path := range [][]string{{"audit"}, {"net-helper"}} {
		c, _, err := root.Find(path)
		if err != nil || IsGroup(c) {
			t.Errorf("brig %v: %v, group %v; want a command that runs", path, err, IsGroup(c))
		}
	}
	for _, path := range [][]string{{"sandbox"}, {"completion"}} {
		c, _, err := root.Find(path)
		if err != nil || !IsGroup(c) {
			t.Errorf("brig %v: %v, group %v; want a group", path, err, c != nil && IsGroup(c))
		}
	}
	if !IsGroup(root) {
		t.Error("brig is no group")
	}
}

func TestReportNamesUsageErrors(t *testing.T) {
	var format outputFormat
	cmd := &cobra.Command{Use: "x"}
	addOutputFlag(cmd, &format)
	if err := cmd.ParseFlags([]string{"-o", "json"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := report(cmd, cobratree.Usagef("takes no argument"), false, &stdout, &stderr)
	var got jsonError
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if code != exitUsage || got.Reason != "usage" || got.ExitCode != exitUsage || !strings.Contains(stderr.String(), "takes no argument") {
		t.Errorf("code %d, JSON %+v, stderr %q", code, got, stderr.String())
	}
}
