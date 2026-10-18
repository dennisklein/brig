// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

func TestSandboxCreateUsesTheVMsSandboxMount(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	src := t.TempDir()
	v.Mounts = []vm.Mount{
		{Source: src, Target: "/mnt/rw", Tag: "brig0"},
		{Source: src, Target: "/mnt/kit", ReadOnly: true, Sandbox: true, Tag: "brig1"},
	}
	v.NextMountTag = 2
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENSHELL_SANDBOX_POLICY", "/other/vm/policy.yaml")
	got := captureExec(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "sandbox", "create", "dev", "-m", link+":/usr/local/share/kit", "--", "--name", "kit", "--", "pi", "-g")
	if err != nil {
		t.Fatalf("brig sandbox create: %v: %s", err, out)
	}
	want := []string{
		"openshell", "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "create", "--driver-config-json",
		`{"podman":{"mounts":[{"type":"volume","source":"brig1","target":"/usr/local/share/kit"}]}}`,
		"--no-auto-providers", "--name", "kit", "--", "pi", "-g",
	}
	if !slices.Equal(*got, want) {
		t.Errorf("exec %q\nwant %q", *got, want)
	}
	// The VM has no default policy, so another VM's does not apply.
	if p, ok := os.LookupEnv("OPENSHELL_SANDBOX_POLICY"); ok {
		t.Errorf("OPENSHELL_SANDBOX_POLICY = %q", p)
	}
	if again, _ := a.vms.Load("dev"); len(again.Mounts) != 2 {
		t.Errorf("mounts changed: %+v", again.Mounts)
	}
}

func TestSandboxCreateTakesTheVMFromTheEnvironment(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	got := captureExec(t)
	t.Setenv("OPENSHELL_GATEWAY", "brig-dev")
	if out, err := run(t, "sandbox", "create"); err != nil {
		t.Fatalf("brig sandbox create: %v: %s", err, out)
	}
	if want := []string{"openshell", "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "create", "--no-auto-providers"}; !slices.Equal(*got, want) {
		t.Errorf("exec %q, want %q", *got, want)
	}
	t.Setenv("OPENSHELL_GATEWAY", "other")
	if _, err := run(t, "sandbox", "create"); err == nil || !strings.Contains(err.Error(), "no brig VM's") {
		t.Errorf("another gateway: %v", err)
	}
	t.Setenv("OPENSHELL_GATEWAY", "")
	if _, err := run(t, "sandbox", "create"); err == nil || !strings.Contains(err.Error(), "brig use") {
		t.Errorf("no gateway: %v", err)
	}
}

func TestSandboxCreateInAWorkspace(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	got := captureExec(t)
	if out, err := run(t, "sandbox", "create", "dev", "--workspace", "team", "--", "--name", "kit", "--", "x", "--workspace", "y"); err != nil {
		t.Fatalf("brig sandbox create: %v: %s", err, out)
	}
	want := []string{"openshell", "openshell", "-g", "brig-dev", "--workspace", "team", "sandbox", "create", "--no-auto-providers", "--name", "kit", "--", "x", "--workspace", "y"}
	if !slices.Equal(*got, want) {
		t.Errorf("exec %q\nwant %q", *got, want)
	}
}

func TestSandboxCreateAttachesOnlyTheProvidersNamed(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	for _, tc := range []struct {
		pass []string
		want []string
	}{
		// What brig ui runs: no second --no-auto-providers.
		{[]string{"--name", "kit", "--provider", "llm", "--detach", "--no-auto-providers"}, []string{"--name", "kit", "--provider", "llm", "--detach", "--no-auto-providers"}},
		{[]string{"--auto-providers", "--provider", "github"}, []string{"--auto-providers", "--provider", "github"}},
		// The sandbox's command may take the flag for itself.
		{[]string{"--name", "kit", "--", "x", "--auto-providers"}, []string{"--no-auto-providers", "--name", "kit", "--", "x", "--auto-providers"}},
	} {
		got := captureExec(t)
		if out, err := run(t, append([]string{"sandbox", "create", "dev", "--"}, tc.pass...)...); err != nil {
			t.Fatalf("brig sandbox create: %v: %s", err, out)
		}
		want := append([]string{"openshell", "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "create"}, tc.want...)
		if !slices.Equal(*got, want) {
			t.Errorf("exec %q\nwant %q", *got, want)
		}
	}
}

func TestSandboxCreateRejects(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	captureExec(t)
	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"dev", "web"}, "at most one VM name"},
		{[]string{"dev", "-m", "/src"}, "want DIR:TARGET"},
		{[]string{"dev", "-m", "/src:/a:ro"}, "want DIR:TARGET"},
		{[]string{"dev", "-m", "/src:a"}, "clean absolute path"},
		{[]string{"dev", "-m", "/src:/"}, "clean absolute path"},
		{[]string{"dev", "--", "-g", "brig-web"}, "picks the gateway"},
		{[]string{"dev", "--", "--gateway=brig-web"}, "picks the gateway"},
		{[]string{"dev", "--", "--driver-config-json", "{}"}, "sets it from --mount"},
		{[]string{"dev", "--", "--workspace", "team"}, "give it to brig"},
		{[]string{"dev", "--", "--workspace=team"}, "give it to brig"},
		{[]string{"dev", "--workspace", "--name"}, "invalid workspace name"},
		{[]string{"dev", "--workspace", "team", "-m", "/src:/a"}, "only in workspace default"},
	} {
		if _, err := run(t, append([]string{"sandbox", "create"}, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("sandbox create %q: %v, want an error containing %q", tc.args, err, tc.wantErr)
		}
	}
}

func TestPlanSandboxMounts(t *testing.T) {
	v := &vm.VM{
		Mounts: []vm.Mount{
			{Source: "/src/a", Target: "/mnt/a", Tag: "brig0"},
			{Source: "/src/b", Target: "/mnt/b", ReadOnly: true, Sandbox: true, Tag: "brig3"},
		},
		NextMountTag: 4,
	}
	specs := []sandboxMount{{"/src/a", "/x"}, {"/src/b", "/y"}, {"/src/a", "/z"}}
	mounts, volumes, added := planSandboxMounts(v, specs)
	if !added || !slices.Equal(volumes, []string{"brig4", "brig3", "brig4"}) {
		t.Fatalf("volumes %q, added %v", volumes, added)
	}
	// A plain mount of the same directory is no sandbox mount; the new one
	// gets a tag that was never used.
	want := vm.Mount{Source: "/src/a", Target: "/mnt/brig4", ReadOnly: true, Sandbox: true, Tag: "brig4"}
	if len(mounts) != 3 || mounts[2] != want {
		t.Errorf("mounts %+v, want a third %+v", mounts, want)
	}
	if len(v.Mounts) != 2 || v.NextMountTag != 4 {
		t.Errorf("planSandboxMounts changed the VM: %+v", v)
	}
	if _, _, added := planSandboxMounts(v, specs[1:2]); added {
		t.Error("planned a mount for a directory the VM has a sandbox mount of")
	}
}

func TestSandboxCreateConnect(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	started := false
	origStart, origRun := startIfStopped, runCommand
	startIfStopped = func(context.Context, *app, *vm.VM, io.Writer) error { started = true; return nil }
	var ran []string
	runCommand = func(_ context.Context, argv []string) error { ran = argv; return nil }
	t.Cleanup(func() { startIfStopped, runCommand = origStart, origRun })
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": {{Workspace: "default", Name: "kit", Phase: "Ready"}}})
	got := captureExec(t)
	if out, err := run(t, "sandbox", "create", "dev", "--connect", "--", "--name=kit", "--template", "pi"); err != nil {
		t.Fatalf("brig sandbox create --connect: %v: %s", err, out)
	}
	wantRun := []string{"openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "create", "--no-auto-providers", "--detach", "--name=kit", "--template", "pi"}
	if !started || !slices.Equal(ran, wantRun) {
		t.Errorf("started %v, ran %q\nwant %q", started, ran, wantRun)
	}
	if want := []string{"openshell", "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "connect", "kit"}; !slices.Equal(*got, want) {
		t.Errorf("exec %q, want %q", *got, want)
	}
	if _, err := run(t, "sandbox", "create", "dev", "--connect", "--", "--template", "pi", "--", "--name", "x"); err == nil || !strings.Contains(err.Error(), "needs the sandbox's --name") {
		t.Errorf("--connect without --name: %v", err)
	}
}

func TestWaitSandboxReady(t *testing.T) {
	defer func(d time.Duration) { sandboxReadyTimeout = d }(sandboxReadyTimeout)
	sandboxReadyTimeout = 300 * time.Millisecond
	v := &vm.VM{Name: "dev"}
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": {
		{Workspace: "default", Name: "up", Phase: "Ready"},
		{Workspace: "default", Name: "slow", Phase: "Provisioning"},
		{Workspace: "default", Name: "bad", Phase: "Error", Problem: "ConfigurationInvalid"},
	}})
	if err := waitSandboxReady(t.Context(), v, "default", "up"); err != nil {
		t.Errorf("ready: %v", err)
	}
	if err := waitSandboxReady(t.Context(), v, "default", "slow"); exitStatus(err) != exitNotReady || !strings.Contains(err.Error(), "is provisioning") {
		t.Errorf("provisioning: %v", err)
	}
	if err := waitSandboxReady(t.Context(), v, "default", "bad"); exitStatus(err) != exitNotReady || !strings.Contains(err.Error(), "ConfigurationInvalid") {
		t.Errorf("in error: %v", err)
	}
}
