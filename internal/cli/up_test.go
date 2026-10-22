// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

func TestUpRefuses(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	for _, tc := range []struct {
		args    []string
		code    int
		message string
	}{
		{[]string{"nosuch"}, exitNotFound, "brig up --create creates it"},
		{[]string{"dev", "--", "--cpus", "2"}, exitUsage, "are for --create"},
		{[]string{"dev", "web"}, exitUsage, "at most one VM name"},
		// The flags after -- reach brig create.
		{[]string{"new", "--create", "--", "--cpus", "0"}, exitFailure, "at least 1 CPU"},
		{[]string{"new", "--create", "--", "--no-start"}, exitFailure, "none of the others"},
	} {
		out, err := run(t, append([]string{"up"}, tc.args...)...)
		if code := exitStatus(err); code != tc.code || err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("brig up %q: exit status %d, %v (%s); want %d and %q", tc.args, code, err, out, tc.code, tc.message)
		}
	}
	if a.vms.Exists("new") {
		t.Error("a failed create left a VM")
	}
}

func TestUpListsSandboxesAndPendingRules(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": testSandboxes})
	orig := gatewayDrafts
	gatewayDrafts = func(_ context.Context, _ *vm.VM, rows []sandboxRow) ([]int, error) {
		out := make([]int, len(rows))
		out[0], out[2] = 3, 1
		return out, nil
	}
	t.Cleanup(func() { gatewayDrafts = orig })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(new(bytes.Buffer))
	if err := a.printUp(t.Context(), cmd, v, "table"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"WORKSPACE  NAME",
		"3 drafted rules wait for review in sandbox webapp: openshell -g brig-dev --workspace default rule get webapp --status pending, or brig ui.",
		"1 drafted rule waits for review in sandbox kit: openshell -g brig-dev --workspace team rule get kit --status pending",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, &out)
		}
	}
	if strings.Contains(out.String(), "sandbox old:") {
		t.Errorf("a sandbox without drafts is listed as waiting:\n%s", &out)
	}

	out.Reset()
	if err := a.printUp(t.Context(), cmd, v, "json"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		VM        struct{ Name string }
		Sandboxes []upSandbox
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.VM.Name != "dev" || len(got.Sandboxes) != 3 ||
		got.Sandboxes[0].PendingRules != 3 || got.Sandboxes[0].Name != "webapp" || got.Sandboxes[1].PendingRules != 0 {
		t.Errorf("%v: %s", err, &out)
	}

	fakeSandboxes(t, map[string][]store.Sandbox{"dev": nil})
	out.Reset()
	if err := a.printUp(t.Context(), cmd, v, "table"); err != nil || !strings.Contains(out.String(), "has no sandboxes yet") {
		t.Errorf("%v: %s", err, &out)
	}
}

func TestPrintPendingEscapesGatewayNames(t *testing.T) {
	var out bytes.Buffer
	evil := "w\x1b]52;c;cm0gLXJmIH4=\a"
	err := printPending(&out, &vm.VM{Name: "dev"}, []upSandbox{
		{sandboxRow: sandboxRow{Workspace: evil, Name: "web\x1b[2Japp"}, PendingRules: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\a") || !strings.Contains(out.String(), "1 drafted rule waits") {
		t.Errorf("printPending wrote %q", &out)
	}
}
