// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/openshell"
)

var teamSecrets = fakeSecrets{"service github.com user alice": "tok-default"}

// twoWorkspaceDir defines a github profile and provider in the default
// workspace and in team.
func twoWorkspaceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, base := range []string{dir, filepath.Join(dir, "workspaces", "team")} {
		writeFile(t, filepath.Join(base, "profiles", "github.yaml"), githubProfile)
		writeFile(t, filepath.Join(base, "providers", "github.yaml"), githubProvider)
	}
	return dir
}

func TestLoadWorkspaces(t *testing.T) {
	dir := twoWorkspaceDir(t)
	writeFile(t, filepath.Join(dir, "workspaces", "empty", "keep"), "")
	set, err := Load([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := set.WorkspaceNames(); !slices.Equal(got, []string{"default", "empty", "team"}) {
		t.Errorf("workspaces %q", got)
	}
	for _, name := range []string{"default", "team"} {
		ws := set.Workspaces[name]
		if len(ws.Profiles) != 1 || len(ws.Providers) != 1 || ws.Providers[0].Name != "github" {
			t.Errorf("workspace %s: %+v", name, ws)
		}
	}
	if want := filepath.Join(dir, "workspaces", "team", "providers", "github.yaml"); set.Workspaces["team"].Providers[0].Path != want {
		t.Errorf("team provider path %q", set.Workspaces["team"].Providers[0].Path)
	}

	// The same name twice in one workspace is an error, also across directories.
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "workspaces", "team", "providers", "github.yaml"), githubProvider)
	if _, err := Load([]string{dir, other}); err == nil || !strings.Contains(err.Error(), "provider github is defined in") {
		t.Errorf("duplicate within a workspace: %v", err)
	}
	// Another workspace of another directory is fine.
	writeFile(t, filepath.Join(other, "workspaces", "lab", "providers", "github.yaml"), githubProvider)
	if err := os.RemoveAll(filepath.Join(other, "workspaces", "team")); err != nil {
		t.Fatal(err)
	}
	if set, err := Load([]string{dir, other}); err != nil || len(set.Workspaces["lab"].Providers) != 1 {
		t.Errorf("another workspace: %v", err)
	}
	// Workspace names are validated.
	bad := t.TempDir()
	writeFile(t, filepath.Join(bad, "workspaces", "-x", "providers", "github.yaml"), githubProvider)
	if _, err := Load([]string{bad}); err == nil || !strings.Contains(err.Error(), "invalid workspace name") {
		t.Errorf("bad name: %v", err)
	}
}

func TestSyncSameProviderInTwoWorkspaces(t *testing.T) {
	dir := twoWorkspaceDir(t)
	gw, st := newFakeGateway(), &State{}
	out, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"lint github.yaml", "import github", "create github github map[GH_TOKEN:tok-default]",
		"create-workspace team",
		"team: lint github.yaml", "team: import github", "team: create github github map[GH_TOKEN:tok-default]",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls %q, want %q", calls, want)
	}
	if len(gw.providers) != 1 || len(gw.others["team"].providers) != 1 {
		t.Errorf("providers: default %d, team %d", len(gw.providers), len(gw.others["team"].providers))
	}
	i, j := strings.Index(out, "workspace default:\n"), strings.Index(out, "workspace team:\n")
	if i < 0 || j < i || !strings.Contains(out[j:], "provider github: created") {
		t.Errorf("headings:\n%s", out)
	}

	// The state is per workspace; the workspace is recorded as brig's.
	if st.Version != 0 || !dflt(st).Providers["github"].Created || !st.Workspaces["team"].Providers["github"].Created {
		t.Errorf("state %+v", st)
	}
	if !st.Workspaces["team"].Created || dflt(st).Created {
		t.Errorf("created flags: default %v, team %v", dflt(st).Created, st.Workspaces["team"].Created)
	}

	// Nothing changed: nothing to do.
	if out, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{}); err != nil || len(calls) != 0 || out != "" {
		t.Errorf("second sync: calls %q, report %q, %v", calls, out, err)
	}
}

func TestSyncOnlyDefaultHasNoHeading(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), githubProfile)
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	out, calls, err := syncDir(t, dir, newFakeGateway(), teamSecrets, &State{}, Options{})
	if err != nil || strings.Contains(out, "workspace default") || slices.Contains(calls, "list-workspaces") {
		t.Errorf("report %q, calls %q, %v", out, calls, err)
	}
}

func TestSyncPruneStaysInItsWorkspace(t *testing.T) {
	dir := twoWorkspaceDir(t)
	gw, st := newFakeGateway(), &State{}
	if _, _, err := syncDir(t, dir, gw, teamSecrets, st, Options{}); err != nil {
		t.Fatal(err)
	}
	// Team's directory disappears; default's stays.
	if err := os.RemoveAll(filepath.Join(dir, "workspaces")); err != nil {
		t.Fatal(err)
	}
	out, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"team: delete github", "team: delete-profile github"}; !slices.Equal(calls, want) {
		t.Errorf("calls %q, want %q", calls, want)
	}
	if len(gw.providers) != 1 || len(gw.profiles) != 1 || len(gw.others["team"].providers) != 0 {
		t.Errorf("default %d providers, %d profiles; team %d providers", len(gw.providers), len(gw.profiles), len(gw.others["team"].providers))
	}
	if !strings.Contains(out, "workspace team:\nprovider github: deleted") {
		t.Errorf("report:\n%s", out)
	}
	// The workspace is kept, with its record.
	if ws := st.Workspaces["team"]; ws == nil || !ws.Created || len(ws.Providers) != 0 {
		t.Errorf("team state %+v", ws)
	}
	if slices.ContainsFunc(gw.calls, func(c string) bool { return strings.Contains(c, "delete-workspace") }) {
		t.Error("a workspace was deleted")
	}
	// Another sync keeps it too.
	if _, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{Prune: true}); err != nil || len(calls) != 0 || st.Workspaces["team"] == nil {
		t.Errorf("later sync: calls %q, %v", calls, err)
	}

	// An object that default's state does not know is never pruned from team.
	gw.in("team").providers["manual"] = &fakeProvider{typ: "x", creds: map[string]string{}}
	if _, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{Prune: true}); err != nil || len(calls) != 0 {
		t.Errorf("manual provider: calls %q, %v", calls, err)
	}
}

func TestSyncWorkspaceGoneFromTheGateway(t *testing.T) {
	gw := newFakeGateway()
	st := &State{Workspaces: map[string]*WorkspaceState{"team": {Created: true, Providers: map[string]ProviderState{"github": {Type: "github", Created: true}}}}}
	_, calls, err := syncDir(t, t.TempDir(), gw, teamSecrets, st, Options{Prune: true})
	if err != nil || len(calls) != 0 || st.Workspaces["team"] != nil {
		t.Errorf("calls %q, state %+v, %v", calls, st.Workspaces, err)
	}
}

func TestSyncDryRunCreatesNoWorkspace(t *testing.T) {
	dir := twoWorkspaceDir(t)
	gw, st := newFakeGateway(), &State{}
	out, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(calls, "create-workspace team") || len(gw.others) != 0 || !strings.Contains(out, "workspace team: would create") {
		t.Errorf("calls %q, report:\n%s", calls, out)
	}
}

func TestSyncSelectedWorkspaces(t *testing.T) {
	dir := twoWorkspaceDir(t)
	gw, st := newFakeGateway(), &State{}
	out, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{Workspaces: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(calls, "import github") || len(gw.providers) != 0 || len(gw.others["team"].providers) != 1 || strings.Contains(out, "workspace default") {
		t.Errorf("calls %q, report:\n%s", calls, out)
	}
	if _, _, err := syncDir(t, dir, gw, teamSecrets, st, Options{Workspaces: []string{"nosuch"}}); err == nil || !strings.Contains(err.Error(), "workspace nosuch is not defined") {
		t.Errorf("unknown workspace: %v", err)
	}
}

func TestSyncErrorsNameTheirWorkspace(t *testing.T) {
	dir := twoWorkspaceDir(t)
	gw := newFakeGateway()
	gw.in("team").failOn = "import"
	_, _, err := syncDir(t, dir, gw, teamSecrets, &State{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "workspace team: profile github") || strings.Contains(err.Error(), "workspace default") {
		t.Errorf("error: %v", err)
	}
	// The default workspace was synced all the same.
	if len(gw.providers) != 1 {
		t.Errorf("default providers %d", len(gw.providers))
	}
}

func TestSyncHoldsBackPerWorkspace(t *testing.T) {
	dir := twoWorkspaceDir(t)
	gw, st := newFakeGateway(), &State{}
	if _, _, err := syncDir(t, dir, gw, teamSecrets, st, Options{HoldNewEndpoints: true}); err != nil {
		t.Fatal(err)
	}
	// Only the default workspace has a provider of type github now.
	gw.in("team").providers = map[string]*fakeProvider{}
	wide := githubProfile + "  - host: collector.example.net\n    port: 443\n"
	writeFile(t, filepath.Join(dir, "workspaces", "team", "profiles", "github.yaml"), wide)
	if _, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{HoldNewEndpoints: true}); err != nil || !slices.Contains(calls, "team: update-profile github") {
		t.Errorf("team has no credentials to protect: calls %q, %v", calls, err)
	}
	// In default, a provider holds the credentials.
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), wide)
	gw.profiles["github"] = openshell.Profile{ID: "github", Scope: openshell.ScopeWorkspace, ResourceVersion: gw.profiles["github"].ResourceVersion}
	_, calls, err := syncDir(t, dir, gw, teamSecrets, st, Options{HoldNewEndpoints: true})
	if err == nil || !strings.Contains(err.Error(), "held back") || slices.Contains(calls, "update-profile github") {
		t.Errorf("default: calls %q, %v", calls, err)
	}
	if strings.Contains(err.Error(), "workspace team") {
		t.Errorf("team was held back too: %v", err)
	}
}

func TestStateIsPerWorkspaceAndVersioned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// The flat layout of earlier versions is not read; its key is.
	writeFile(t, path, `{"key": "AQID", "profiles": {"github": {"sha256": "x", "created": true}}, "providers": {"github": {"type": "github", "credentials": {}, "created": true}}}`)
	st, err := LoadState(path)
	if err != nil || len(st.Workspaces) != 0 || len(st.Key) != 3 {
		t.Fatalf("old state: %+v, %v", st, err)
	}
	dflt(st).Providers["github"] = ProviderState{Type: "github", Created: true}
	st.Workspace("team").Created = true
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{`"version": 2`, `"workspaces"`, `"team"`, `"default"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("state file lacks %s:\n%s", want, data)
		}
	}
	back, err := LoadState(path)
	if err != nil || !back.Workspaces["default"].Providers["github"].Created || !back.Workspaces["team"].Created {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}
