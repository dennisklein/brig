// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUseNeedsTheShellIntegration(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	t.Setenv(shellIntegrationEnv, "")
	t.Setenv("SHELL", "/usr/bin/zsh")
	out, err := run(t, "use", "dev")
	if err == nil || !strings.Contains(err.Error(), `eval "$(brig shell-init zsh)"`) {
		t.Errorf("use without the integration: %v, want a pointer to brig shell-init zsh", err)
	}
	if out != "" {
		t.Errorf("use printed %q, want nothing", out)
	}
	t.Setenv(shellIntegrationEnv, "csh")
	if _, err := run(t, "use", "dev"); err == nil || !strings.Contains(err.Error(), "sh or fish") {
		t.Errorf("use for csh: %v", err)
	}
}

func TestUsePrintsWhatEnvPrints(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.OpenShellConfigs = []string{configDir(t)}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	for _, sh := range []string{"sh", "fish"} {
		t.Setenv(shellIntegrationEnv, sh)
		for _, args := range [][]string{{"dev"}, {"--unset"}} {
			want, err := run(t, append([]string{"env", "--shell", sh}, args...)...)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := run(t, append([]string{"use"}, args...)...); err != nil || got != want {
				t.Errorf("%s: use %q = %q, %v; want %q", sh, args, got, err, want)
			}
		}
	}
	for _, args := range [][]string{{}, {"dev", "--unset"}} {
		if _, err := run(t, append([]string{"use"}, args...)...); err == nil {
			t.Errorf("use %q succeeded", args)
		}
	}
}

// The brig function evaluates what brig use writes to standard output, so
// its help must go elsewhere.
func TestUseHelpGoesToStandardError(t *testing.T) {
	cmd := newRootCmd(&progressRun{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"use", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("use --help wrote %q to stdout and %q to stderr", stdout.String(), stderr.String())
	}
}

func TestShellInitRejectsOtherShells(t *testing.T) {
	if _, err := run(t, "shell-init", "csh"); err == nil {
		t.Error("shell-init csh succeeded")
	}
}

// TestShellInit runs brig use through the brig function in each shell, with a
// brig that prints what the real one would.
func TestShellInit(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	dir := configDir(t)
	v.OpenShellConfigs = []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(dir, "policies", "default.yaml")
	outs := t.TempDir()
	for _, sh := range []string{"sh", "fish"} {
		t.Setenv(shellIntegrationEnv, sh)
		for name, args := range map[string][]string{"dev": {"dev"}, "unset": {"--unset"}} {
			out, err := run(t, append([]string{"use"}, args...)...)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(outs, sh, name), out, 0o600)
		}
	}
	t.Setenv(shellIntegrationEnv, "")
	t.Setenv("BRIG_TEST_OUTS", outs)
	fakeCommands(t, map[string]string{"brig": `
if [ "$1" != use ]; then printf 'ran %s|' "$*"; exit 0; fi
case $2 in
dev) exec cat "$BRIG_TEST_OUTS/$BRIG_SHELL_INTEGRATION/dev" ;;
--unset) exec cat "$BRIG_TEST_OUTS/$BRIG_SHELL_INTEGRATION/unset" ;;
*) echo 'export OPENSHELL_GATEWAY=wrong' >&2; exit 3 ;;
esac`})

	posix := `brig use dev; printf '%s|%s|' "$OPENSHELL_GATEWAY" "$OPENSHELL_SANDBOX_POLICY"
brig use nope; printf '%s|%s|' "$?" "$OPENSHELL_GATEWAY"
brig list -o json
brig use --unset; printf '%s|%s|' "${OPENSHELL_GATEWAY-unset}" "${OPENSHELL_SANDBOX_POLICY-unset}"`
	want := "brig-dev|" + policy + "|3|brig-dev|ran list -o json|unset|unset|"
	for _, tc := range []struct {
		shell string
		argv  []string
		test  string
	}{
		{"bash", []string{"--norc", "-c"}, posix},
		{"zsh", []string{"-f", "-c"}, posix},
		{"fish", []string{"--no-config", "-c"}, `brig use dev; printf '%s|%s|' "$OPENSHELL_GATEWAY" "$OPENSHELL_SANDBOX_POLICY"
brig use nope; printf '%s|%s|' "$status" "$OPENSHELL_GATEWAY"
brig list -o json
brig use --unset; set -q OPENSHELL_GATEWAY; or printf 'unset|'; set -q OPENSHELL_SANDBOX_POLICY; or printf 'unset|'`},
	} {
		path, err := exec.LookPath(tc.shell)
		if err != nil {
			t.Logf("skipping %s: %v", tc.shell, err)
			continue
		}
		integration, err := run(t, "shell-init", tc.shell)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(path, append(tc.argv, integration+"\n"+tc.test)...)
		cmd.Env = append(os.Environ(), "OPENSHELL_GATEWAY=", "OPENSHELL_SANDBOX_POLICY=")
		got, err := cmd.Output()
		if err != nil {
			t.Errorf("%s: %v", tc.shell, err)
		}
		if string(got) != want {
			t.Errorf("%s saw %q, want %q", tc.shell, got, want)
		}
	}
}

// TestP10kSegment checks the zsh integration's Powerlevel10k segment with a
// p10k that prints how it was called.
func TestP10kSegment(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("no zsh")
	}
	integration, err := run(t, "shell-init", "zsh")
	if err != nil {
		t.Fatal(err)
	}
	script := integration + `
p10k() { print -rn -- "$*|" }
for OPENSHELL_GATEWAY in brig-dev brig- other ''; do prompt_brig; instant_prompt_brig; done; true`
	got, err := exec.Command(zsh, "-f", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "segment -f 208 -i brig -t dev|segment -f 208 -i brig -t dev|"; string(got) != want {
		t.Errorf("segments = %q, want %q", got, want)
	}
}
