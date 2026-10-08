// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/openshell"
)

// fakeGateway keeps profiles and providers in memory and records calls.
type fakeGateway struct {
	profiles  map[string]openshell.Profile
	files     map[string]string // profile ID -> content of the last imported or updated file
	providers map[string]*fakeProvider
	calls     []string
	failOn    string
}

type fakeProvider struct {
	typ   string
	creds map[string]string
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{profiles: map[string]openshell.Profile{}, files: map[string]string{}, providers: map[string]*fakeProvider{}}
}

func (g *fakeGateway) call(format string, args ...any) error {
	c := fmt.Sprintf(format, args...)
	g.calls = append(g.calls, c)
	if g.failOn != "" && strings.HasPrefix(c, g.failOn) {
		return errors.New("gateway says no")
	}
	return nil
}

func (g *fakeGateway) Profiles(context.Context) ([]openshell.Profile, error) {
	return slices.Collect(maps.Values(g.profiles)), nil
}

func (g *fakeGateway) LintProfile(_ context.Context, file string) error {
	return g.call("lint %s", filepath.Base(file))
}

func (g *fakeGateway) store(id, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	g.files[id] = string(data)
	p := g.profiles[id]
	p.ID, p.Scope, p.ResourceVersion = id, openshell.ScopeWorkspace, p.ResourceVersion+1
	g.profiles[id] = p
	return nil
}

func (g *fakeGateway) ImportProfile(_ context.Context, file string) error {
	id := strings.TrimSuffix(filepath.Base(file), ".yaml")
	if err := g.call("import %s", id); err != nil {
		return err
	}
	return g.store(id, file)
}

func (g *fakeGateway) UpdateProfile(_ context.Context, id, file string) error {
	if err := g.call("update-profile %s", id); err != nil {
		return err
	}
	// Like OpenShell, accept only the current resource_version.
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if want := fmt.Sprintf("resource_version: %d\n", g.profiles[id].ResourceVersion); !strings.Contains(string(data), want) {
		return fmt.Errorf("stale resource_version, want %q in:\n%s", want, data)
	}
	return g.store(id, file)
}

func (g *fakeGateway) DeleteProfile(_ context.Context, id string) error {
	delete(g.profiles, id)
	return g.call("delete-profile %s", id)
}

func (g *fakeGateway) Providers(context.Context) ([]openshell.Provider, error) {
	var out []openshell.Provider
	for name, p := range g.providers {
		out = append(out, openshell.Provider{Name: name, Type: p.typ, CredentialKeys: slices.Sorted(maps.Keys(p.creds))})
	}
	return out, nil
}

func (g *fakeGateway) CreateProvider(_ context.Context, name, typ string, creds map[string]string) error {
	if err := g.call("create %s %s %v", name, typ, creds); err != nil {
		return err
	}
	g.providers[name] = &fakeProvider{typ: typ, creds: maps.Clone(creds)}
	return nil
}

func (g *fakeGateway) UpdateProvider(_ context.Context, name string, creds map[string]string, remove []string) error {
	if err := g.call("update %s %v remove %v", name, creds, remove); err != nil {
		return err
	}
	p := g.providers[name]
	maps.Copy(p.creds, creds)
	for _, k := range remove {
		delete(p.creds, k)
	}
	return nil
}

func (g *fakeGateway) DeleteProvider(_ context.Context, name string) error {
	delete(g.providers, name)
	return g.call("delete %s", name)
}

// fakeSecrets serves secrets by their joined attributes.
type fakeSecrets map[string]string

func (s fakeSecrets) Lookup(_ context.Context, attrs []string) (string, error) {
	v, ok := s[strings.Join(attrs, " ")]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

// syncDir runs a sync of dir and returns its report and the gateway calls.
func syncDir(t *testing.T, dir string, gw *fakeGateway, sec Secrets, st *State, opts Options) (string, []string, error) {
	t.Helper()
	set, err := Load([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	opts.Out = &out
	gw.calls = nil
	err = Sync(context.Background(), set, gw, sec, st, opts)
	return out.String(), gw.calls, err
}

func TestSyncLifecycle(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), "resource_version: 7\n"+githubProfile)
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	gw, st := newFakeGateway(), &State{}
	sec := fakeSecrets{"service github.com user alice": "tok1"}

	out, calls, err := syncDir(t, dir, gw, sec, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lint github.yaml", "import github", "create github github map[GH_TOKEN:tok1]"}; !slices.Equal(calls, want) {
		t.Errorf("first sync calls %q, want %q", calls, want)
	}
	if strings.Contains(gw.files["github"], "resource_version") {
		t.Errorf("imported file keeps the resource_version:\n%s", gw.files["github"])
	}
	if !strings.Contains(out, "provider github: created (type github); GH_TOKEN may be sent to api.github.com:443, github.com:443") {
		t.Errorf("report:\n%s", out)
	}
	if strings.Contains(out, "tok1") {
		t.Error("the report shows a secret")
	}

	// Nothing changed: nothing to do, nothing to report.
	out, calls, err = syncDir(t, dir, gw, sec, st, Options{})
	if err != nil || len(calls) != 0 || out != "" {
		t.Errorf("second sync: calls %q, report %q, %v", calls, out, err)
	}
	if out, _, _ := syncDir(t, dir, gw, sec, st, Options{Verbose: true}); !strings.Contains(out, "provider github: up to date") {
		t.Errorf("verbose report:\n%s", out)
	}

	// A rotated secret and an edited profile.
	sec["service github.com user alice"] = "tok2"
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), githubProfile+"description: edited\n")
	_, calls, err = syncDir(t, dir, gw, sec, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lint github.yaml", "update-profile github", "update github map[GH_TOKEN:tok2] remove []"}; !slices.Equal(calls, want) {
		t.Errorf("third sync calls %q, want %q", calls, want)
	}
	if !strings.Contains(gw.files["github"], "resource_version: 1\n") {
		t.Errorf("update lacks the gateway's resource_version:\n%s", gw.files["github"])
	}

	// --refresh-secrets updates unchanged secrets too.
	if _, calls, _ := syncDir(t, dir, gw, sec, st, Options{RefreshSecrets: true}); !slices.Equal(calls, []string{"update github map[GH_TOKEN:tok2] remove []"}) {
		t.Errorf("refresh calls %q", calls)
	}

	// A credential dropped from the provider file is removed.
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider+"  OTHER:\n    secret_tool:\n      lookup: [k, v]\n")
	sec["k v"] = "o"
	_, _, _ = syncDir(t, dir, gw, sec, st, Options{})
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	out, calls, _ = syncDir(t, dir, gw, sec, st, Options{})
	if !slices.Equal(calls, []string{"update github map[GH_TOKEN:tok2] remove [OTHER]"}) || !strings.Contains(out, "removes OTHER") {
		t.Errorf("credential removal: calls %q, report %q", calls, out)
	}

	// Gone from the directory: kept without --prune, deleted with it.
	if err := os.RemoveAll(filepath.Join(dir, "providers")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "profiles")); err != nil {
		t.Fatal(err)
	}
	out, calls, _ = syncDir(t, dir, gw, sec, st, Options{})
	if len(calls) != 0 || strings.Count(out, "no longer configured; kept") != 2 {
		t.Errorf("without prune: calls %q, report %q", calls, out)
	}
	if out, calls, _ = syncDir(t, dir, gw, sec, st, Options{Prune: true, DryRun: true}); len(calls) != 0 || !strings.Contains(out, "would delete") {
		t.Errorf("prune dry run: calls %q, report %q", calls, out)
	}
	_, calls, _ = syncDir(t, dir, gw, sec, st, Options{Prune: true})
	if want := []string{"delete github", "delete-profile github"}; !slices.Equal(calls, want) {
		t.Errorf("prune calls %q, want %q", calls, want)
	}
	if len(st.Profiles)+len(st.Providers) != 0 || len(gw.providers)+len(gw.profiles) != 0 {
		t.Errorf("after prune: state %+v, gateway %+v %+v", st, gw.providers, gw.profiles)
	}
}

func TestSyncDryRunChangesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), githubProfile)
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	gw, st := newFakeGateway(), &State{}
	out, calls, err := syncDir(t, dir, gw, fakeSecrets{"service github.com user alice": "t"}, st, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"lint github.yaml"}) || len(st.Profiles)+len(st.Providers) != 0 {
		t.Errorf("dry run: calls %q, state %+v", calls, st)
	}
	for _, want := range []string{"profile github: would import", "provider github: would create (type github)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestSyncCarriesOnAfterFailures(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "providers", "a.yaml"), strings.Replace(githubProvider, "name: github", "name: a", 1))
	writeFile(t, filepath.Join(dir, "providers", "b.yaml"), "name: b\ntype: nosuch\ncredentials:\n  T:\n    secret_tool: {lookup: [x, y]}\n")
	writeFile(t, filepath.Join(dir, "providers", "c.yaml"), "name: c\ntype: github\ncredentials:\n  T:\n    secret_tool: {lookup: [missing, secret]}\n")
	writeFile(t, filepath.Join(dir, "providers", "d.yaml"), strings.Replace(githubProvider, "name: github", "name: d", 1))
	gw := newFakeGateway()
	gw.profiles["github"] = openshell.Profile{ID: "github", Scope: openshell.ScopePlatform, Endpoints: []openshell.Endpoint{{Host: "api.github.com", Port: 443}}}
	gw.providers["d"] = &fakeProvider{typ: "anthropic", creds: map[string]string{}}
	st := &State{}
	out, calls, err := syncDir(t, dir, gw, fakeSecrets{"service github.com user alice": "t"}, st, Options{})
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"unknown provider type nosuch", "no such secret", "with type anthropic, not github"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if !slices.Equal(calls, []string{"create a github map[GH_TOKEN:t]"}) {
		t.Errorf("calls %q", calls)
	}
	if !strings.Contains(out, "provider a: created (type github); GH_TOKEN may be sent to api.github.com:443") {
		t.Errorf("report:\n%s", out)
	}
	if _, ok := st.Providers["a"]; !ok || len(st.Providers) != 1 {
		t.Errorf("state %+v", st.Providers)
	}
}

func TestSyncAdoptsAnExistingProvider(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	gw := newFakeGateway()
	gw.profiles["github"] = openshell.Profile{ID: "github", Scope: openshell.ScopePlatform}
	gw.providers["github"] = &fakeProvider{typ: "github", creds: map[string]string{"GH_TOKEN": "old", "MANUAL": "m"}}
	st := &State{}
	out, calls, err := syncDir(t, dir, gw, fakeSecrets{"service github.com user alice": "new"}, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Credentials brig did not set stay.
	if !slices.Equal(calls, []string{"update github map[GH_TOKEN:new] remove []"}) {
		t.Errorf("calls %q", calls)
	}
	if !strings.Contains(out, "sandbox policies bind") {
		t.Errorf("an endpointless profile's report:\n%s", out)
	}
}

func TestSyncDoesNotPruneWhatItDidNotCreate(t *testing.T) {
	gw := newFakeGateway()
	gw.profiles["manual"] = openshell.Profile{ID: "manual", Scope: openshell.ScopeWorkspace}
	gw.providers["manual"] = &fakeProvider{typ: "manual", creds: map[string]string{}}
	if _, calls, err := syncDir(t, t.TempDir(), gw, fakeSecrets{}, &State{}, Options{Prune: true}); err != nil || len(calls) != 0 {
		t.Errorf("prune of an empty set: calls %q, %v", calls, err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := LoadState(path)
	if err != nil || st.Key != nil {
		t.Fatalf("missing state: %+v, %v", st, err)
	}
	fp1, _ := st.fingerprint("p", "K", "secret")
	fp2, _ := st.fingerprint("p", "K", "secret")
	fp3, _ := st.fingerprint("p", "K", "other")
	if fp1 != fp2 || fp1 == fp3 || strings.Contains(fp1, "secret") {
		t.Errorf("fingerprints %s %s %s", fp1, fp2, fp3)
	}
	st.Providers = map[string]ProviderState{"p": {Type: "t", Credentials: map[string]string{"K": fp1}}}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	back, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if fp, _ := back.fingerprint("p", "K", "secret"); fp != fp1 {
		t.Error("the key did not survive saving")
	}
}

func TestSyncNeverDeletesWhatItTookOver(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), githubProfile)
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	gw := newFakeGateway()
	gw.profiles["github"] = openshell.Profile{
		ID: "github", Scope: openshell.ScopeWorkspace, ResourceVersion: 4,
		Endpoints: []openshell.Endpoint{{Host: "api.github.com", Port: 443}, {Host: "github.com", Port: 443}},
	}
	gw.providers["github"] = &fakeProvider{typ: "github", creds: map[string]string{"GH_TOKEN": "old", "MANUAL": "m"}}
	st := &State{}
	sec := fakeSecrets{"service github.com user alice": "new"}
	_, calls, err := syncDir(t, dir, gw, sec, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lint github.yaml", "update-profile github", "update github map[GH_TOKEN:new] remove []"}; !slices.Equal(calls, want) {
		t.Errorf("take-over calls %q, want %q", calls, want)
	}
	if err := os.RemoveAll(filepath.Join(dir, "providers")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "profiles")); err != nil {
		t.Fatal(err)
	}
	out, calls, err := syncDir(t, dir, gw, sec, st, Options{Prune: true})
	if err != nil || len(calls) != 0 || strings.Count(out, "since brig did not create it") != 2 {
		t.Errorf("prune of taken-over resources: calls %q, report %q, %v", calls, out, err)
	}
	if len(st.Profiles)+len(st.Providers) != 0 || gw.providers["github"].creds["MANUAL"] != "m" {
		t.Errorf("state %+v, gateway provider %+v", st, gw.providers["github"])
	}
}

func TestSyncPruneSkipsAProviderOfAnotherType(t *testing.T) {
	gw := newFakeGateway()
	gw.providers["github"] = &fakeProvider{typ: "gitlab", creds: map[string]string{}}
	st := &State{Providers: map[string]ProviderState{"github": {Type: "github", Created: true}}}
	out, calls, err := syncDir(t, t.TempDir(), gw, fakeSecrets{}, st, Options{Prune: true})
	if err != nil || len(calls) != 0 || !strings.Contains(out, "type gitlab now") || len(st.Providers) != 0 {
		t.Errorf("calls %q, report %q, state %+v, %v", calls, out, st.Providers, err)
	}
}

func TestSyncHoldsBackNewEndpointsForExistingCredentials(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), githubProfile)
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	gw, st := newFakeGateway(), &State{}
	sec := fakeSecrets{"service github.com user alice": "t"}
	// The first import and create are not held back: no credentials exist yet.
	if _, _, err := syncDir(t, dir, gw, sec, st, Options{HoldNewEndpoints: true}); err != nil {
		t.Fatal(err)
	}
	gw.profiles["github"] = openshell.Profile{
		ID: "github", Scope: openshell.ScopeWorkspace, ResourceVersion: gw.profiles["github"].ResourceVersion,
		Endpoints: []openshell.Endpoint{{Host: "api.github.com", Port: 443}, {Host: "github.com", Port: 443}},
	}
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), githubProfile+"  - host: collector.example.net\n    port: 443\n")

	out, calls, err := syncDir(t, dir, gw, sec, st, Options{HoldNewEndpoints: true})
	if err == nil || !strings.Contains(err.Error(), "held back") || !strings.Contains(err.Error(), "collector.example.net:443") {
		t.Errorf("held back: %v", err)
	}
	if slices.Contains(calls, "update-profile github") || !strings.Contains(out, "providers github also go to collector.example.net:443") {
		t.Errorf("held back: calls %q, report %q", calls, out)
	}

	out, calls, err = syncDir(t, dir, gw, sec, st, Options{})
	if err != nil || !slices.Contains(calls, "update-profile github") || !strings.Contains(out, "may now also go to collector.example.net:443") {
		t.Errorf("explicit sync: calls %q, report %q, %v", calls, out, err)
	}
}

// slowSecrets times out on the first lookup and records all lookups.
type slowSecrets struct{ lookups []string }

func (s *slowSecrets) Lookup(_ context.Context, attrs []string) (string, error) {
	s.lookups = append(s.lookups, strings.Join(attrs, " "))
	return "", fmt.Errorf("secret-tool lookup: %w", context.DeadlineExceeded)
}

func TestSyncStopsLookingUpAfterATimeout(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), githubProfile)
	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(dir, "providers", name+".yaml"), strings.Replace(githubProvider, "name: github", "name: "+name, 1))
	}
	sec := &slowSecrets{}
	_, _, err := syncDir(t, dir, newFakeGateway(), sec, &State{}, Options{})
	if err == nil || len(sec.lookups) != 1 || strings.Count(err.Error(), "not looked up") != 2 {
		t.Errorf("lookups %q, error %v", sec.lookups, err)
	}
}
