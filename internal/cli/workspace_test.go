// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWorkspaceFlagAndEnvironment(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	got := captureExec(t)
	for _, tc := range []struct {
		env  string
		args []string
		want string
	}{
		{"", []string{"sandbox", "create", "dev"}, "default"},
		{"", []string{"sandbox", "create", "dev", "-w", "team"}, "team"},
		{"team", []string{"sandbox", "create", "dev"}, "team"},
		{"team", []string{"sandbox", "create", "dev", "--workspace", "ops"}, "ops"},
	} {
		t.Setenv(workspaceEnv, tc.env)
		if out, err := run(t, tc.args...); err != nil {
			t.Fatalf("%q with BRIG_WORKSPACE=%q: %v: %s", tc.args, tc.env, err, out)
		}
		if i := slices.Index(*got, "--workspace"); i < 0 || (*got)[i+1] != tc.want {
			t.Errorf("%q with BRIG_WORKSPACE=%q: exec %q, want workspace %s", tc.args, tc.env, *got, tc.want)
		}
	}
	t.Setenv(workspaceEnv, "Not_Valid")
	if _, err := run(t, "sandbox", "create", "dev"); err == nil || !strings.Contains(err.Error(), workspaceEnv+": invalid workspace name") {
		t.Errorf("an invalid BRIG_WORKSPACE: %v", err)
	}
	// A valid -w makes the variable irrelevant.
	if out, err := run(t, "sandbox", "create", "dev", "-w", "team"); err != nil {
		t.Errorf("-w over an invalid BRIG_WORKSPACE: %v: %s", err, out)
	}
}

func TestEverywhereWorkspaceHasAShortFlag(t *testing.T) {
	root := newRootCmd(&progressRun{})
	for _, path := range [][]string{{"sync"}, {"sandbox", "create"}, {"policy", "diff"}, {"policy", "save"}, {"use"}, {"env"}} {
		c, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if f := c.Flags().Lookup("workspace"); f == nil || f.Shorthand != "w" {
			t.Errorf("brig %v: --workspace = %+v, want -w", path, f)
		}
	}
}

func TestUseExportsTheWorkspace(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	t.Setenv(shellIntegrationEnv, "sh")
	out, err := run(t, "use", "dev", "-w", "team")
	if want := "export OPENSHELL_GATEWAY=brig-dev\nunset OPENSHELL_SANDBOX_POLICY\nexport BRIG_WORKSPACE=team\n"; err != nil || out != want {
		t.Errorf("use dev -w team = %q, %v; want %q", out, err, want)
	}
	// Another VM has other workspaces, so a switch drops the variable.
	out, err = run(t, "use", "dev")
	if err != nil || !strings.HasSuffix(out, "unset BRIG_WORKSPACE\n") {
		t.Errorf("use dev = %q, %v", out, err)
	}
	out, err = run(t, "use", "--unset")
	if want := "unset OPENSHELL_GATEWAY OPENSHELL_SANDBOX_POLICY BRIG_WORKSPACE\n"; err != nil || out != want {
		t.Errorf("use --unset = %q, %v; want %q", out, err, want)
	}
	t.Setenv(shellIntegrationEnv, "fish")
	if out, err := run(t, "env", "dev", "-w", "team", "--shell", "fish"); err != nil || !strings.HasSuffix(out, "set -gx BRIG_WORKSPACE team\n") {
		t.Errorf("env dev -w team = %q, %v", out, err)
	}
	for _, args := range [][]string{{"dev", "-w", "Not_Valid"}, {"--unset", "-w", "team"}} {
		if out, err := run(t, append([]string{"use"}, args...)...); err == nil {
			t.Errorf("use %q succeeded: %q", args, out)
		}
	}
}

func TestUseWorkspaceAloneKeepsTheVM(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	t.Setenv(shellIntegrationEnv, "sh")
	t.Setenv(gatewayEnv, "brig-dev")
	out, err := run(t, "use", "-w", "team")
	if err != nil || !strings.HasPrefix(out, "export OPENSHELL_GATEWAY=brig-dev\n") || !strings.HasSuffix(out, "export BRIG_WORKSPACE=team\n") {
		t.Errorf("use -w team = %q, %v", out, err)
	}
	t.Setenv(gatewayEnv, "")
	if _, err := run(t, "use", "-w", "team"); err == nil {
		t.Error("use -w without a selected VM succeeded")
	}
	// Asking tells the workspace too.
	t.Setenv(gatewayEnv, "brig-dev")
	t.Setenv(workspaceEnv, "team")
	if _, stderr, _, err := runSplit(t, "use"); err != nil || !strings.Contains(stderr, "VM dev, workspace team") {
		t.Errorf("use = %q, %v", stderr, err)
	}
}

func TestCompleteWorkspacesAndPolicyFiles(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	dir := configDir(t)
	writeTestFile(t, filepath.Join(dir, "workspaces", "team", "providers", "gl.yaml"), "name: gl\ntype: github\ncredentials:\n  T:\n    secret_tool:\n      lookup: [service, gitlab]\n", 0o600)
	writeTestFile(t, filepath.Join(dir, "policies", "webapp.yaml"), "version: 1\n", 0o600)
	v.OpenShellConfigs = []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	t.Setenv(gatewayEnv, "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"sync", "dev", "--workspace", ""}, "default\nteam\n:4\n"},
		{[]string{"sandbox", "create", "dev", "-w", ""}, "default\nteam\n:4\n"},
		{[]string{"policy", "diff", "dev", "web", "-w", ""}, "default\nteam\n:4\n"},
		{[]string{"policy", "diff", "dev", "web", "--source", ""}, "default\nwebapp\n:4\n"},
		{[]string{"policy", "save", "dev", "web", "--to", ""}, "default\nwebapp\n:4\n"},
	} {
		out, err := run(t, append([]string{"__complete"}, tc.args...)...)
		if err != nil || !strings.HasPrefix(out, tc.want) {
			t.Errorf("complete %q = %q, %v; want %q", tc.args, out, err, tc.want)
		}
	}
	// Without a VM named, the selected one is used.
	t.Setenv(gatewayEnv, "brig-dev")
	out, err := run(t, "__complete", "sync", "-w", "")
	if err != nil || !strings.HasPrefix(out, "default\nteam\n:4\n") {
		t.Errorf("complete -w for the selected VM = %q, %v", out, err)
	}
	out, err = run(t, "__complete", "policy", "diff", "--source", "")
	if err != nil || !strings.HasPrefix(out, "default\nwebapp\n:4\n") {
		t.Errorf("complete --source for the selected VM = %q, %v", out, err)
	}
}
