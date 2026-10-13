// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"slices"
	"strings"
	"testing"
)

// vmCommands are the commands that work on one VM and take its name before
// anything else.
var vmCommands = [][]string{
	{"start"},
	{"stop"},
	{"delete"},
	{"show"},
	{"update"},
	{"upgrade"},
	{"sync"},
	{"console"},
	{"ssh"},
	{"image", "push", "localhost/img"},
	{"sandbox", "create"},
	{"policy", "diff", "web"},
	{"policy", "save", "web"},
}

func TestVMCommandsNeedAVMOrBrigUse(t *testing.T) {
	testApp(t)
	captureExec(t)
	t.Setenv(gatewayEnv, "")
	for _, args := range vmCommands {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "brig use NAME") {
			t.Errorf("brig %q: %v, want a pointer to brig use", args, err)
		}
	}
	t.Setenv(gatewayEnv, "elsewhere")
	for _, args := range vmCommands {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "no brig VM's") {
			t.Errorf("brig %q with another gateway: %v", args, err)
		}
	}
	// A selected VM that is gone is reported by name.
	t.Setenv(gatewayEnv, "brig-gone")
	for _, args := range vmCommands {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "gone") {
			t.Errorf("brig %q with a deleted VM: %v", args, err)
		}
	}
}

func TestVMCommandsFollowBrigUse(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	testVM(t, a, "other")
	t.Setenv(gatewayEnv, "brig-dev")

	// show names the VM it resolved on standard error only.
	out, errOut, _, err := runSplit(t, "show", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"name": "dev"`) || strings.Contains(out, "Using VM") {
		t.Errorf("brig show -o json printed %q", out)
	}
	if !strings.Contains(errOut, "Using VM dev (selected by brig use).") {
		t.Errorf("brig show on standard error: %q", errOut)
	}
	// A named VM wins and is not announced.
	out, errOut, _, err = runSplit(t, "show", "other", "-o", "json")
	if err != nil || !strings.Contains(out, `"name": "other"`) || strings.Contains(errOut, "Using VM") {
		t.Errorf("brig show other: %q, %q, %v", out, errOut, err)
	}
	if _, _, _, err := runSplit(t, "show", "dev", "other"); err == nil {
		t.Error("brig show took two VMs")
	}
}

func TestSSHTakesTheVMBeforeTheCommandOnly(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	got := captureExec(t)
	t.Setenv(gatewayEnv, "brig-dev")
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"ssh", "--", "journalctl", "-b"}, []string{"agent@127.0.0.1", "journalctl", "-b"}},
		{[]string{"ssh", "dev", "--", "journalctl", "-b"}, []string{"agent@127.0.0.1", "journalctl", "-b"}},
		{[]string{"ssh", "dev", "uptime"}, []string{"agent@127.0.0.1", "uptime"}},
		{[]string{"ssh"}, []string{"agent@127.0.0.1"}},
	} {
		*got = nil
		if out, err := run(t, tc.args...); err != nil {
			t.Fatalf("brig %q: %v: %s", tc.args, err, out)
		}
		argv := *got
		dash := slices.Index(argv, "--")
		if dash < 0 || !slices.Equal(argv[dash+1:], tc.want) {
			t.Errorf("brig %q: exec %q, want %q after --", tc.args, argv, tc.want)
		}
	}
	if _, err := run(t, "ssh", "dev", "other", "--", "ls"); err == nil || !strings.Contains(err.Error(), "at most one VM") {
		t.Errorf("two VMs: %v", err)
	}
}

func TestUseWithoutNameTellsTheVM(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	// Not even the shell integration is needed to ask.
	t.Setenv(shellIntegrationEnv, "")
	t.Setenv(gatewayEnv, "brig-dev")
	out, errOut, started, err := runSplit(t, "use")
	if err != nil || out != "" || started || !strings.Contains(errOut, "points at VM dev") {
		t.Errorf("brig use = %q, %q, %v, %v", out, errOut, started, err)
	}
	t.Setenv(gatewayEnv, "")
	if _, _, _, err := runSplit(t, "use"); err == nil || !strings.Contains(err.Error(), "points at no VM") {
		t.Errorf("brig use with no VM selected: %v", err)
	}
	t.Setenv(gatewayEnv, "elsewhere")
	if _, _, _, err := runSplit(t, "use"); err == nil || !strings.Contains(err.Error(), "no brig VM's") {
		t.Errorf("brig use with another gateway: %v", err)
	}
	if _, err := run(t, "env"); err == nil || !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("brig env without a name: %v", err)
	}
}
