// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/vm"
)

func TestRecreateMovesSandboxToTheTemplatesImage(t *testing.T) {
	g := newFakeSnapGW()
	old := g.add("webapp", "Ready", "localhost/claude-agent:1", "work")
	s, a, _ := recreateEngine(t, g)
	ctx := context.Background()
	p, err := s.planRecreate(ctx, "default", "webapp", recreateTarget{template: "claude"}, planOpts{resets: []string{".pi/agent/settings.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.ToImage != "localhost/claude-agent:2" || !slices.Equal(p.Steps, []string{"stop", "snapshot", "delete", "create", "restore", "start"}) {
		t.Errorf("plan %+v", p)
	}
	snap, err := s.recreate(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.Sandbox(ctx, "default", "webapp")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == old.ID || r.Phase != "Ready" || r.Image != "localhost/claude-agent:2" || r.Template != "claude" || r.Labels["app"] != "webapp" {
		t.Errorf("new sandbox %+v", r)
	}
	if got := g.volumes[r.Volume()]; got != "work" {
		t.Errorf("new /sandbox %q", got)
	}
	c := g.created[0]
	if c.Template != "claude" || string(c.Policy) != `{"networkPolicies":{"npm":{"name":"npm"}}}` || c.Annotations[recreatedFrom] != "localhost/claude-agent:1" ||
		len(c.Services) != 1 {
		t.Errorf("create %+v", c)
	}
	if !strings.HasPrefix(snap.Name(), "webapp@2026-10-14T15:02:1") {
		t.Errorf("snapshot %s", snap.Name())
	}
	if _, ok := g.snaps[snap.path()]; !ok {
		t.Errorf("snapshot %s is gone", snap.path())
	}
	if j, err := a.readJournal(s.v, "default", "webapp"); j != nil || err != nil {
		t.Errorf("journal left: %+v, %v", j, err)
	}
	// The restore leaves out the reset path.
	if len(g.restores) != 1 || !slices.Equal(g.restores[0], []string{snap.path(), ".pi/agent/settings.json"}) {
		t.Errorf("restores %q", g.restores)
	}
}

func TestRecreateWithThePolicyFile(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Stopped", "localhost/claude-agent:1", "work")
	s, _, _ := recreateEngine(t, g)
	orig := filePolicy
	filePolicy = func(_ context.Context, _ *app, _ *vm.VM, ws, name, choice string) (string, json.RawMessage, error) {
		if ws != "default" || name != "webapp" || choice != "file" {
			t.Errorf("filePolicy %s %s %s", ws, name, choice)
		}
		return "policies/webapp.yaml", json.RawMessage(`{"version":1}`), nil
	}
	t.Cleanup(func() { filePolicy = orig })
	ctx := context.Background()
	p, err := s.planRecreate(ctx, "default", "webapp", recreateTarget{image: "localhost/claude-agent:2"}, planOpts{policy: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Policy != "policy file policies/webapp.yaml" || !slices.Equal(p.Steps, []string{"snapshot", "delete", "create", "restore"}) {
		t.Errorf("plan %+v", p)
	}
	if _, err := s.recreate(ctx, p); err != nil {
		t.Fatal(err)
	}
	c := g.created[0]
	if string(c.Policy) != `{"version":1}` || c.Template != "" || c.Image != "localhost/claude-agent:2" {
		t.Errorf("create %+v", c)
	}
	if r, _ := g.Sandbox(ctx, "default", "webapp"); r.Phase != "Stopped" || r.Image != "localhost/claude-agent:2" {
		t.Errorf("sandbox %+v", r)
	}
}

// TestRecreateFailures fails each step of a recreate in turn and checks
// that the sandbox ends up where it was, with its /sandbox.
func TestRecreateFailures(t *testing.T) {
	for _, tc := range []struct {
		fail map[string]int
		// image is the sandbox's image afterwards; rolled says that a
		// rollback created it again.
		image  string
		rolled bool
	}{
		{fail: map[string]int{"stop": 1}, image: "localhost/claude-agent:1"},
		{fail: map[string]int{"snapshot": 1}, image: "localhost/claude-agent:1"},
		{fail: map[string]int{"delete": 1}, image: "localhost/claude-agent:1"},
		{fail: map[string]int{"create": 1}, image: "localhost/claude-agent:1", rolled: true},
		{fail: map[string]int{"restore": 1}, image: "localhost/claude-agent:1", rolled: true},
		// The first start is the new sandbox's after the restore.
		{fail: map[string]int{"start": 1}, image: "localhost/claude-agent:1", rolled: true},
	} {
		t.Run(fmt.Sprint(tc.fail), func(t *testing.T) {
			g := newFakeSnapGW()
			old := g.add("webapp", "Ready", "localhost/claude-agent:1", "work")
			s, a, errw := recreateEngine(t, g)
			ctx := context.Background()
			p, err := s.planRecreate(ctx, "default", "webapp", recreateTarget{template: "claude"}, planOpts{})
			if err != nil {
				t.Fatal(err)
			}
			maps.Copy(g.fail, tc.fail)
			if _, err := s.recreate(ctx, p); err == nil {
				t.Fatal("recreate succeeded")
			}
			r, err := g.Sandbox(ctx, "default", "webapp")
			if err != nil {
				t.Fatalf("the sandbox is gone: %v\n%s", err, errw)
			}
			if r.Phase != "Ready" || r.Image != tc.image || g.volumes[r.Volume()] != "work" || (r.ID != old.ID) != tc.rolled {
				t.Errorf("sandbox %+v with /sandbox %q, calls %q\n%s", r, g.volumes[r.Volume()], g.calls, errw)
			}
			if tc.rolled && (g.created[len(g.created)-1].Template != "" || string(g.created[len(g.created)-1].Policy) == "") {
				t.Errorf("the rollback created %+v", g.created[len(g.created)-1])
			}
			if j, err := a.readJournal(s.v, "default", "webapp"); j != nil || err != nil {
				t.Errorf("journal left: %+v, %v", j, err)
			}
			for rel := range g.snaps {
				if !strings.HasPrefix(rel, "default/webapp/") {
					t.Errorf("snapshot %s", rel)
				}
			}
		})
	}
}

func TestRecreateWhoseRollbackFailsCanResume(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Stopped", "localhost/claude-agent:1", "work")
	s, a, _ := recreateEngine(t, g)
	ctx := context.Background()
	p, err := s.planRecreate(ctx, "default", "webapp", recreateTarget{template: "claude"}, planOpts{})
	if err != nil {
		t.Fatal(err)
	}
	g.fail["create"] = 2
	_, err = s.recreate(ctx, p)
	if err == nil || !strings.Contains(err.Error(), "--resume") || !strings.Contains(err.Error(), "webapp@") {
		t.Fatalf("recreate: %v", err)
	}
	if _, err := g.Sandbox(ctx, "default", "webapp"); !errors.Is(err, osclient.ErrNoSandbox) {
		t.Fatalf("the sandbox: %v", err)
	}
	j, err := a.readJournal(s.v, "default", "webapp")
	if err != nil || j == nil || j.Phase != "deleted" {
		t.Fatalf("journal %+v, %v", j, err)
	}
	// The journal is a lock.
	if _, err := s.recreate(ctx, p); exitStatus(err) != exitExists {
		t.Errorf("a second recreate: %v", err)
	}
	if snaps, err := s.list(ctx, false); err != nil || len(snaps) != 1 || !snaps[0].Recreate || !snaps[0].Gone {
		t.Errorf("snapshots %+v, %v", snaps, err)
	}
	if _, err := s.forward(ctx, j); err != nil {
		t.Fatal(err)
	}
	r, err := g.Sandbox(ctx, "default", "webapp")
	if err != nil || r.Image != "localhost/claude-agent:2" || r.Phase != "Stopped" || g.volumes[r.Volume()] != "work" {
		t.Errorf("sandbox %+v, %v", r, err)
	}
	if j, err := a.readJournal(s.v, "default", "webapp"); j != nil || err != nil {
		t.Errorf("journal left: %+v, %v", j, err)
	}
}

func TestRecreateRollbackByHand(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Ready", "localhost/claude-agent:1", "work")
	s, a, _ := recreateEngine(t, g)
	ctx := context.Background()
	p, err := s.planRecreate(ctx, "default", "webapp", recreateTarget{template: "claude"}, planOpts{})
	if err != nil {
		t.Fatal(err)
	}
	g.fail["restore"] = 2
	if _, err := s.recreate(ctx, p); err == nil {
		t.Fatal("recreate succeeded")
	}
	j, err := a.readJournal(s.v, "default", "webapp")
	if err != nil || j == nil {
		t.Fatalf("journal %+v, %v", j, err)
	}
	if err := s.back(ctx, j); err != nil {
		t.Fatal(err)
	}
	r, err := g.Sandbox(ctx, "default", "webapp")
	if err != nil || r.Image != "localhost/claude-agent:1" || r.Phase != "Ready" || g.volumes[r.Volume()] != "work" {
		t.Errorf("sandbox %+v, %v", r, err)
	}
}

func TestRecreateInterruptedRollsBack(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Ready", "localhost/claude-agent:1", "work")
	s, _, _ := recreateEngine(t, g)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := s.planRecreate(ctx, "default", "webapp", recreateTarget{template: "claude"}, planOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// Ctrl-C while the new sandbox is created.
	g.interrupt, g.cancel = "create", cancel
	if _, err := s.recreate(ctx, p); err == nil {
		t.Fatal("recreate succeeded")
	}
	r, err := g.Sandbox(context.Background(), "default", "webapp")
	if err != nil || r.Image != "localhost/claude-agent:1" || r.Phase != "Ready" || g.volumes[r.Volume()] != "work" {
		t.Errorf("sandbox %+v, %v", r, err)
	}
}

func TestRecreatePlanRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(g *fakeSnapGW)
		target recreateTarget
		want   string
	}{
		"missing provider": {func(g *fakeSnapGW) { g.providers = nil }, recreateTarget{template: "claude"}, "brig sync"},
		"missing image":    {func(*fakeSnapGW) {}, recreateTarget{image: "localhost/other:1"}, "brig image push"},
		"old image gone":   {func(g *fakeSnapGW) { delete(g.images, "localhost/claude-agent:1") }, recreateTarget{template: "claude"}, "rollback"},
		"ext4":             {func(g *fakeSnapGW) { g.fs = "ext2/ext3" }, recreateTarget{template: "claude"}, "recreate the VM"},
		"no template":      {func(*fakeSnapGW) {}, recreateTarget{template: "pi"}, "no template pi"},
		"no volume":        {func(g *fakeSnapGW) { clear(g.volumes) }, recreateTarget{template: "claude"}, "no workspace volume"},
		"in error":         {func(g *fakeSnapGW) { g.sandboxes["default/webapp"].Phase = "Error" }, recreateTarget{template: "claude"}, "is error"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newFakeSnapGW()
			g.add("webapp", "Ready", "localhost/claude-agent:1", "work")
			s, _, _ := recreateEngine(t, g)
			tc.change(g)
			_, err := s.planRecreate(context.Background(), "default", "webapp", tc.target, planOpts{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("plan: %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSandboxRecreateDryRun(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Ready", "localhost/claude-agent:1", "work")
	g.ps = "self 50\n1 0 openshell-sandb\n7 1 bash\n214 7 claude\n50 7 sh\n51 50 cat\n"
	testApp(t)
	fakeRecreate(t, g)
	a, _ := newApp()
	testVM(t, a, "dev")
	out, err := run(t, "sandbox", "recreate", "dev", "webapp", "--dry-run")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := `Sandbox webapp (workspace default), ready
  image     localhost/claude-agent:1 -> localhost/claude-agent:2 (template claude)
  providers claude-code (all present)
  policy    the sandbox's current base policy, the same as policies/default.yaml
  services  web:8080, exposed again
  /sandbox  4B; a snapshot shares its blocks
  running   claude (pid 214): will be stopped
  steps     stop, snapshot, delete, create, restore, start
`
	if out != want {
		t.Errorf("dry run printed\n%s\nwant\n%s", out, want)
	}
	if len(g.created) != 0 || len(g.snaps) != 0 || slices.Contains(g.calls, "stop") {
		t.Errorf("the dry run changed something: %q", g.calls)
	}

	// Without a terminal, a running agent needs --yes.
	out, err = run(t, "sandbox", "recreate", "dev", "webapp")
	if err == nil || !strings.Contains(err.Error(), "claude (pid 214)") || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("recreate: %v: %s", err, out)
	}
	out, err = run(t, "sandbox", "recreate", "dev", "webapp", "--yes")
	if err != nil || !strings.Contains(out, "Moved sandbox webapp from localhost/claude-agent:1 to localhost/claude-agent:2; snapshot webapp@") {
		t.Errorf("recreate --yes: %v: %s", err, out)
	}
}

func TestAgentUpdatePicksTheTemplatesSandboxes(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Ready", "localhost/claude-agent:1", "w")
	g.add("api", "Stopped", "localhost/claude-agent:1", "a")
	g.add("fresh", "Ready", "localhost/claude-agent:2", "f")
	g.add("run-1", "Ready", "localhost/claude-agent:1", "r").Labels[runLabel] = "1"
	other := g.add("other", "Ready", "localhost/claude-agent:1", "o")
	other.Template = "pi"
	s, _, _ := recreateEngine(t, g)
	var out bytes.Buffer
	cmd := newAgentUpdateCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	items, err := agentSandboxes(cmd, s, "default", "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		names = append(names, it.name)
	}
	if !slices.Equal(names, []string{"api", "webapp"}) {
		t.Errorf("picked %q", names)
	}
	if _, err := agentSandboxes(cmd, s, "default", "claude", []string{"other"}); err == nil || !strings.Contains(err.Error(), "--template claude") {
		t.Errorf("another template's sandbox: %v", err)
	}
	if items, err := agentSandboxes(cmd, s, "default", "claude", []string{"fresh"}); err != nil || len(items) != 1 {
		t.Errorf("a named sandbox: %v, %v", items, err)
	}
}

func TestAgentUpdateStopsAtTheFirstFailure(t *testing.T) {
	g := newFakeSnapGW()
	g.add("api", "Stopped", "localhost/claude-agent:1", "a")
	g.add("webapp", "Stopped", "localhost/claude-agent:1", "w")
	g.add("zeta", "Stopped", "localhost/claude-agent:1", "z")
	s, a, _ := recreateEngine(t, g)
	var out bytes.Buffer
	cmd := newAgentUpdateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetContext(context.Background())
	// The second sandbox's snapshot fails.
	calls := 0
	orig := vmScript
	vmScript = func(ctx context.Context, a *app, v *vm.VM, script string) (string, error) {
		if strings.Contains(script, `copy_tree "$mp" "$tmp/data"`) {
			calls++
			if calls == 2 {
				return "", errors.New("disk full")
			}
		}
		return orig(ctx, a, v, script)
	}
	items := []recreateItem{
		{name: "api", target: recreateTarget{template: "claude"}},
		{name: "webapp", target: recreateTarget{template: "claude"}},
		{name: "zeta", target: recreateTarget{template: "claude"}},
	}
	err := a.recreateAll(cmd, s, "default", items, recreateOpts{})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("update: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "Moved sandbox api ") || !strings.HasPrefix(lines[1], "Did not move sandbox webapp: ") ||
		lines[2] != "Left sandbox zeta as it was." {
		t.Errorf("printed\n%s", out.String())
	}
	for name, image := range map[string]string{"api": "localhost/claude-agent:2", "webapp": "localhost/claude-agent:1", "zeta": "localhost/claude-agent:1"} {
		if r, _ := g.Sandbox(context.Background(), "default", name); r.Image != image {
			t.Errorf("%s is on %s", name, r.Image)
		}
	}
}

// fakeRecreate is fakeSnapshots with a fixed note on the policy.
func fakeRecreate(t *testing.T, g *fakeSnapGW) {
	t.Helper()
	fakeSnapshots(t, g)
	stubPolicyNote(t)
}

// recreateEngine is engine with a fixed note on the policy.
func recreateEngine(t *testing.T, g *fakeSnapGW) (*snapshots, *app, *bytes.Buffer) {
	t.Helper()
	s, a, errw := engine(t, g)
	stubPolicyNote(t)
	return s, a, errw
}

func stubPolicyNote(t *testing.T) {
	t.Helper()
	orig := policyNote
	policyNote = func(context.Context, *app, *vm.VM, string, string) string {
		return ", the same as policies/default.yaml"
	}
	t.Cleanup(func() { policyNote = orig })
}

// engine returns the snapshots of VM dev of a test app.
func engine(t *testing.T, g *fakeSnapGW) (*snapshots, *app, *bytes.Buffer) {
	t.Helper()
	a := testApp(t)
	v := testVM(t, a, "dev")
	fakeSnapshots(t, g)
	var errw bytes.Buffer
	s, err := a.snapshots(context.Background(), v, &errw)
	if err != nil {
		t.Fatal(err)
	}
	return s, a, &errw
}
