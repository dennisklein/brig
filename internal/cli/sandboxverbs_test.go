// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// fakeSandboxes makes each VM's gateway hold the sandboxes that byVM lists
// for it; a VM it does not list has a gateway that does not answer.
func fakeSandboxes(t *testing.T, byVM map[string][]store.Sandbox) {
	t.Helper()
	orig := gatewaySandboxes
	gatewaySandboxes = func(_ context.Context, v *vm.VM) ([]store.Sandbox, error) {
		s, ok := byVM[v.Name]
		if !ok {
			return nil, notReadyf("the gateway of VM %s does not answer", v.Name)
		}
		return s, nil
	}
	t.Cleanup(func() { gatewaySandboxes = orig })
}

var testSandboxes = []store.Sandbox{
	{
		Workspace: "default", Name: "webapp", Phase: "Ready", Image: "localhost/pi-agent:1", Providers: []string{"github", "llm"},
		Created: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC),
	},
	{Workspace: "default", Name: "old", Phase: "Stopped", Template: "pi"},
	{Workspace: "team", Name: "kit", Phase: "Ready"},
}

func TestSandboxVerbsRunOpenShellOnTheVMsGateway(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": testSandboxes})
	gw := []string{"openshell", "openshell", "-g", "brig-dev", "--workspace"}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"connect", "dev", "webapp"}, []string{"default", "sandbox", "connect", "webapp"}},
		{
			[]string{"exec", "dev", "webapp", "--workdir", "/sandbox/w", "--no-tty", "--env", "A=b", "--", "git", "status", "--short"},
			[]string{"default", "sandbox", "exec", "--name", "webapp", "--workdir", "/sandbox/w", "--no-tty", "--env", "A=b", "--", "git", "status", "--short"},
		},
		{
			[]string{"logs", "dev", "old", "-f", "--since", "10m", "--source", "sandbox"},
			[]string{"default", "logs", "old", "-n", "200", "--tail", "--since", "10m", "--source", "sandbox"},
		},
		{[]string{"start", "dev", "old"}, []string{"default", "sandbox", "start", "old"}},
		{[]string{"stop", "dev", "kit", "-w", "team"}, []string{"team", "sandbox", "stop", "kit"}},
		{[]string{"delete", "dev", "old", "--yes"}, []string{"default", "sandbox", "delete", "old"}},
		{[]string{"attach", "dev", "webapp", "gitlab"}, []string{"default", "sandbox", "provider", "attach", "webapp", "gitlab", "--wait"}},
		{[]string{"detach", "dev", "webapp", "llm"}, []string{"default", "sandbox", "provider", "detach", "webapp", "llm", "--wait"}},
	} {
		got := captureExec(t)
		if out, err := run(t, append([]string{"sandbox"}, tc.args...)...); err != nil {
			t.Errorf("brig sandbox %q: %v: %s", tc.args, err, out)
			continue
		}
		if want := append(slices.Clone(gw), tc.want...); !slices.Equal(*got, want) {
			t.Errorf("brig sandbox %q: exec %q\nwant %q", tc.args, *got, want)
		}
	}

	// Without a VM name, brig use's VM.
	t.Setenv("OPENSHELL_GATEWAY", "brig-dev")
	got := captureExec(t)
	if out, err := run(t, "sandbox", "connect", "webapp"); err != nil || !strings.Contains(out, "Using VM dev") {
		t.Fatalf("brig sandbox connect webapp: %v: %s", err, out)
	}
	if want := append(slices.Clone(gw), "default", "sandbox", "connect", "webapp"); !slices.Equal(*got, want) {
		t.Errorf("exec %q, want %q", *got, want)
	}
	// Without -w, brig use's workspace.
	t.Setenv("BRIG_WORKSPACE", "team")
	got = captureExec(t)
	if out, err := run(t, "sandbox", "stop", "kit"); err != nil {
		t.Fatalf("brig sandbox stop kit: %v: %s", err, out)
	}
	if want := append(slices.Clone(gw), "team", "sandbox", "stop", "kit"); !slices.Equal(*got, want) {
		t.Errorf("exec %q, want %q", *got, want)
	}
}

func TestSandboxVerbsCheckTheSandboxFirst(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	testVM(t, a, "down")
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": testSandboxes})
	got := captureExec(t)
	for _, tc := range []struct {
		args    []string
		code    int
		message string
	}{
		{[]string{"connect", "dev", "nosuch"}, exitNotFound, "VM dev has no sandbox nosuch in workspace default"},
		{[]string{"connect", "dev", "kit"}, exitNotFound, "it has one in workspace team"},
		{[]string{"connect", "nosuch", "webapp"}, exitNotFound, "no such VM"},
		{[]string{"connect", "down", "webapp"}, exitNotReady, "does not answer"},
		{[]string{"exec", "dev", "old", "--", "ls"}, exitNotReady, "sandbox old is stopped; start it with brig sandbox start dev old"},
		{[]string{"attach", "dev", "old", "github"}, exitNotReady, "is stopped"},
		{[]string{"detach", "dev", "webapp", "gitlab"}, exitNotFound, "has no provider gitlab attached"},
		{[]string{"exec", "dev", "webapp"}, exitFailure, "give the command after --"},
		{[]string{"connect", "dev", "webapp", "--", "ls"}, exitFailure, "takes no command after --"},
		{[]string{"connect", "dev", "web app"}, exitFailure, "invalid sandbox name"},
		{[]string{"connect", "dev", "webapp", "-w", "Team"}, exitFailure, "invalid workspace name"},
	} {
		out, err := run(t, append([]string{"sandbox"}, tc.args...)...)
		if code := exitStatus(err); code != tc.code || err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("brig sandbox %q: exit status %d, %v (%s); want %d and %q", tc.args, code, err, out, tc.code, tc.message)
		}
	}
	if *got != nil {
		t.Errorf("exec %q after failed checks", *got)
	}
}

func TestSandboxDeleteAsksFirstOnATerminal(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": testSandboxes})
	orig := isInteractive
	t.Cleanup(func() { isInteractive = orig })
	for _, tc := range []struct {
		terminal bool
		args     []string
		answer   string
		exec     bool
		asked    bool
	}{
		{terminal: true, answer: "y\n", exec: true, asked: true},
		{terminal: true, answer: "n\n", asked: true},
		{terminal: true, asked: true},
		{terminal: true, args: []string{"--yes"}, exec: true},
		// A script is not asked, as with brig delete.
		{exec: true},
	} {
		isInteractive = func(io.Reader, io.Writer) bool { return tc.terminal }
		got := captureExec(t)
		cmd := newRootCmd(&progressRun{})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetIn(strings.NewReader(tc.answer))
		cmd.SetArgs(append([]string{"sandbox", "delete", "dev", "old"}, tc.args...))
		err := cmd.Execute()
		asked := strings.Contains(out.String(), "Delete sandbox old of VM dev (workspace default), with everything in its /sandbox? [y/N]")
		if (err == nil) != tc.exec || (*got != nil) != tc.exec || asked != tc.asked {
			t.Errorf("%+v: %v, exec %q\n%s", tc, err, *got, &out)
		}
	}
}

func TestSandboxList(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": testSandboxes})
	out, err := run(t, "sandbox", "list", "dev")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "WORKSPACE  NAME") || !strings.Contains(lines[1], "localhost/pi-agent:1  github,llm") ||
		!strings.Contains(lines[2], "template pi") {
		t.Errorf("table:\n%s", out)
	}
	out, err = run(t, "sandbox", "list", "dev", "-w", "team", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []sandboxRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(rows) != 1 || rows[0].VM != "dev" || rows[0].Name != "kit" || rows[0].Providers == nil {
		t.Errorf("rows %+v", rows)
	}
	if _, err := run(t, "sandbox", "list", "dev", "--all-vms"); err == nil {
		t.Error("a VM name and --all-vms")
	}
}

func TestAllSandboxesListsRunningVMs(t *testing.T) {
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": testSandboxes[:1], "off": testSandboxes[1:]})
	sts := []vmStatus{
		{VM: &vm.VM{Name: "dev"}, State: libvirt.StateRunning.String()},
		{VM: &vm.VM{Name: "off"}, State: libvirt.StateShutoff.String()},
		{VM: &vm.VM{Name: "stuck"}, State: libvirt.StateRunning.String()},
	}
	rows, err := allSandboxes(t.Context(), sts)
	if exitStatus(err) != exitNotReady || !strings.Contains(err.Error(), "stuck") {
		t.Errorf("err = %v", err)
	}
	if len(rows) != 1 || rows[0].VM != "dev" || rows[0].Name != "webapp" {
		t.Errorf("rows %+v", rows)
	}
	var buf bytes.Buffer
	if err := printSandboxes(&buf, rows, "table", true); err != nil || !strings.HasPrefix(buf.String(), "VM   WORKSPACE") {
		t.Errorf("%v\n%s", err, &buf)
	}
	buf.Reset()
	if err := printSandboxes(&buf, nil, "json", true); err != nil || buf.String() != "[]\n" {
		t.Errorf("%v: %q", err, &buf)
	}
}

func TestSandboxDeleteSnapshotFirst(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Ready", "localhost/claude-agent:1", "work")
	testApp(t)
	fakeSnapshots(t, g)
	a, _ := newApp()
	testVM(t, a, "dev")
	origList := gatewaySandboxes
	gatewaySandboxes = func(ctx context.Context, _ *vm.VM) ([]store.Sandbox, error) { return g.Sandboxes(ctx) }
	t.Cleanup(func() { gatewaySandboxes = origList })
	got := captureExec(t)
	out, err := run(t, "sandbox", "delete", "dev", "webapp", "--snapshot")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(g.snaps) != 1 || g.sandboxes["default/webapp"].Phase != "Stopped" || !slices.Contains(*got, "delete") {
		t.Errorf("snapshots %v, exec %q: %s", g.snaps, *got, out)
	}
	if !strings.Contains(out, "Took snapshot webapp@") {
		t.Errorf("output %s", out)
	}
}
