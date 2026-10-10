// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dennisklein/brig/internal/openshell"
)

// Gateway is what a sync needs from an OpenShell gateway;
// *openshell.Gateway implements it.
type Gateway interface {
	// Name is the registered gateway's name, which the CLI takes as -g.
	Name() string
	Profiles(ctx context.Context) ([]openshell.Profile, error)
	LintProfile(ctx context.Context, file string) error
	ImportProfile(ctx context.Context, file string) error
	UpdateProfile(ctx context.Context, id, file string) error
	DeleteProfile(ctx context.Context, id string) error
	Providers(ctx context.Context) ([]openshell.Provider, error)
	CreateProvider(ctx context.Context, name, typ string, creds map[string]string) error
	UpdateProvider(ctx context.Context, name string, creds map[string]string, remove []string) error
	DeleteProvider(ctx context.Context, name string) error
}

// Secrets looks up a secret by its attribute/value pairs; SecretTool
// implements it.
type Secrets interface {
	Lookup(ctx context.Context, attrs []string) (string, error)
}

// Options control a sync.
type Options struct {
	// DryRun reports what the sync would change without changing it.
	// Secrets are still looked up, to tell whether they changed.
	DryRun bool
	// Prune deletes profiles and providers that brig created and that no
	// config directory defines any more.
	Prune bool
	// RefreshSecrets updates every provider's credentials, whether or not
	// they changed.
	RefreshSecrets bool
	// HoldNewEndpoints keeps back profile changes that would let the
	// credentials of providers on the gateway go to more endpoints, so that
	// only an explicit sync widens where credentials may be sent.
	HoldNewEndpoints bool
	// Verbose also reports what is up to date.
	Verbose bool
	// Out receives one line per profile or provider.
	Out io.Writer
}

// Sync makes the gateway's default workspace match set: it imports or
// updates the set's profiles, creates or updates its providers with
// credentials looked up through secrets, and with Prune deletes what brig
// created earlier but the set no longer has. Profiles and providers that
// brig did not create are only touched when the set defines them, and never
// deleted. Sync records what it did in st and carries on after a failed
// step; it returns the errors of all failed steps.
func Sync(ctx context.Context, set *Set, gw Gateway, secrets Secrets, st *State, opts Options) error {
	if st.Profiles == nil {
		st.Profiles = map[string]ProfileState{}
	}
	if st.Providers == nil {
		st.Providers = map[string]ProviderState{}
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	s := &syncer{set: set, gw: gw, secrets: secrets, st: st, opts: opts, unapplied: map[string]bool{}}
	profiles, err := gw.Profiles(ctx)
	if err != nil {
		return err
	}
	providers, err := gw.Providers(ctx)
	if err != nil {
		return err
	}
	s.syncProfiles(ctx, profiles, providers)
	s.syncProviders(ctx, profiles, providers)
	// Providers first: a profile cannot go while a provider uses it.
	s.pruneProviders(ctx, providers)
	s.pruneProfiles(ctx, profiles)
	return errors.Join(s.errs...)
}

type syncer struct {
	set     *Set
	gw      Gateway
	secrets Secrets
	st      *State
	opts    Options
	errs    []error
	// lookupErr stops further secret lookups after one timed out, e.g. at
	// an unanswered keyring unlock prompt.
	lookupErr error
	// unapplied holds the IDs of the set's profiles that this sync did not
	// apply: providers of those types keep their secrets until it does.
	unapplied map[string]bool
}

func (s *syncer) report(kind, name, msg string, changed bool) {
	if changed || s.opts.Verbose {
		fmt.Fprintf(s.opts.Out, "%s %s: %s\n", kind, name, msg)
	}
}

func (s *syncer) fail(kind, name string, err error) {
	fmt.Fprintf(s.opts.Out, "%s %s: failed: %v\n", kind, name, err)
	s.errs = append(s.errs, fmt.Errorf("%s %s: %w", kind, name, err))
}

// done phrases an action for the report: "imported", or "would import" in a
// dry run.
func (s *syncer) done(action string) string {
	if s.opts.DryRun {
		return "would " + action
	}
	return strings.TrimSuffix(action, "e") + "ed"
}

func workspaceProfiles(profiles []openshell.Profile) map[string]openshell.Profile {
	ws := map[string]openshell.Profile{}
	for _, p := range profiles {
		if p.Scope == openshell.ScopeWorkspace {
			ws[p.ID] = p
		}
	}
	return ws
}

// effectiveProfile returns the gateway's profile of the given ID that the
// default workspace uses: its own, else the platform's.
func effectiveProfile(profiles []openshell.Profile, id string) (openshell.Profile, bool) {
	i := slices.IndexFunc(profiles, func(p openshell.Profile) bool { return p.ID == id && p.Scope == openshell.ScopeWorkspace })
	if i < 0 {
		i = slices.IndexFunc(profiles, func(p openshell.Profile) bool { return p.ID == id })
	}
	if i < 0 {
		return openshell.Profile{}, false
	}
	return profiles[i], true
}

// addedEndpoints returns the endpoints of next that cur lacks. An endpoint
// whose path differs counts as another endpoint, since a broader path sends
// the credentials to more requests. A profile that loses all its endpoints
// counts too: sandbox policies can then bind its credentials to any host.
func addedEndpoints(cur, next []openshell.Endpoint) []string {
	if len(cur) > 0 && len(next) == 0 {
		return []string{"any endpoint that sandbox policies bind to it"}
	}
	have := map[string]bool{}
	for _, e := range cur {
		have[e.String()] = true
	}
	var added []string
	for _, e := range next {
		if !have[e.String()] {
			added = append(added, e.String())
		}
	}
	return added
}

func (s *syncer) syncProfiles(ctx context.Context, profiles []openshell.Profile, providers []openshell.Provider) {
	ws := workspaceProfiles(profiles)
	tmp, err := os.MkdirTemp("", "brig-profiles-")
	if err != nil {
		s.errs = append(s.errs, err)
		return
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for _, p := range s.set.Profiles {
		cur, exists := ws[p.ID]
		prev, managed := s.st.Profiles[p.ID]
		if exists && managed && prev.SHA256 == p.SHA256() {
			s.report("profile", p.ID, "up to date", false)
			continue
		}
		// Credentials that the gateway holds already must not silently
		// gain destinations.
		var users []string
		for _, pr := range providers {
			if pr.Type == p.ID {
				users = append(users, pr.Name)
			}
		}
		slices.Sort(users)
		var added []string
		if len(users) > 0 {
			eff, _ := effectiveProfile(profiles, p.ID)
			added = addedEndpoints(eff.Endpoints, p.Endpoints)
		}
		if len(added) > 0 && s.opts.HoldNewEndpoints {
			s.unapplied[p.ID] = true
			s.fail("profile", p.ID, fmt.Errorf("held back: it would let the credentials of providers %s also go to %s; review with `brig sync --dry-run` and apply with `brig sync`",
				strings.Join(users, ", "), strings.Join(added, ", ")))
			continue
		}
		action := "import"
		var version uint64
		if exists {
			action, version = "update", cur.ResourceVersion
		}
		file, err := writeProfile(tmp, p, version)
		// The gateway's lint reports an existing ID as a conflict, so it
		// suits imports only; an update is validated as it is applied.
		if err == nil && !exists {
			err = s.gw.LintProfile(ctx, file)
		}
		if err == nil && !s.opts.DryRun {
			if exists {
				err = s.gw.UpdateProfile(ctx, p.ID, file)
			} else if err = s.gw.ImportProfile(ctx, file); err != nil {
				s.st.Profiles[p.ID] = ProfileState{Created: true}
			}
		}
		if err != nil {
			s.unapplied[p.ID] = true
			s.fail("profile", p.ID, err)
			continue
		}
		if !s.opts.DryRun {
			s.st.Profiles[p.ID] = ProfileState{SHA256: p.SHA256(), Created: !exists || (managed && prev.Created)}
		}
		msg := s.done(action) + " from " + p.Path
		if len(added) > 0 {
			msg += fmt.Sprintf("; the credentials of providers %s may now also go to %s", strings.Join(users, ", "), strings.Join(added, ", "))
		}
		s.report("profile", p.ID, msg, true)
	}
}

// writeProfile writes the profile to dir as the CLI's import and update want
// it: without a resource_version for an import, and with the gateway's
// current one, version, for an update.
func writeProfile(dir string, p ProfileFile, version uint64) (string, error) {
	src, err := yamlSource(p.Path, p.Data)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", p.Path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("%s: not a YAML mapping", p.Path)
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "resource_version" {
			m.Content = slices.Delete(m.Content, i, i+2)
			break
		}
	}
	if version != 0 {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "resource_version"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(version)})
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, p.ID+".yaml")
	return file, os.WriteFile(file, data, 0o600)
}

func (s *syncer) syncProviders(ctx context.Context, profiles []openshell.Profile, providers []openshell.Provider) {
	byName := map[string]openshell.Provider{}
	for _, p := range providers {
		byName[p.Name] = p
	}
	for _, p := range s.set.Providers {
		cur, exists := byName[p.Name]
		if exists && cur.Type != p.Type {
			// Name the gateway: the CLI's active or exported gateway may be another VM's.
			s.fail("provider", p.Name, fmt.Errorf("the gateway has a provider of this name with type %s, not %s; delete it first with `openshell -g %s provider delete %s`", cur.Type, p.Type, s.gw.Name(), p.Name))
			continue
		}
		// The gateway binds credentials to the profile it holds, so new
		// secrets must wait for the profile the set wants.
		if s.unapplied[p.Type] {
			s.fail("provider", p.Name, fmt.Errorf("not applied because profile %s was not applied", p.Type))
			continue
		}
		endpoints, known := s.endpoints(p.Type, profiles)
		if !known {
			s.fail("provider", p.Name, fmt.Errorf("unknown provider type %s; put its profile into a config directory's profiles/", p.Type))
			continue
		}
		secrets, fingerprints, err := s.lookup(ctx, p)
		if err != nil {
			s.fail("provider", p.Name, err)
			continue
		}
		prev, managed := s.st.Providers[p.Name]
		var remove []string
		if exists && managed {
			for key := range prev.Credentials {
				if _, ok := p.Credentials[key]; !ok && slices.Contains(cur.CredentialKeys, key) {
					remove = append(remove, key)
				}
			}
			slices.Sort(remove)
		}
		missing := slices.ContainsFunc(slices.Collect(maps.Keys(p.Credentials)), func(key string) bool {
			return !slices.Contains(cur.CredentialKeys, key)
		})
		if exists && managed && !s.opts.RefreshSecrets && len(remove) == 0 && !missing && maps.Equal(prev.Credentials, fingerprints) {
			s.report("provider", p.Name, "up to date", false)
			continue
		}
		action := "create"
		if exists {
			action = "update"
		}
		if !s.opts.DryRun {
			if exists {
				err = s.gw.UpdateProvider(ctx, p.Name, secrets, remove)
			} else if err = s.gw.CreateProvider(ctx, p.Name, p.Type, secrets); err != nil {
				s.st.Providers[p.Name] = ProviderState{Type: p.Type, Created: true}
			}
			if err != nil {
				s.fail("provider", p.Name, err)
				continue
			}
			s.st.Providers[p.Name] = ProviderState{Type: p.Type, Credentials: fingerprints, Created: !exists || (managed && prev.Created)}
		}
		keys := slices.Sorted(maps.Keys(p.Credentials))
		msg := fmt.Sprintf("%s (type %s); %s may be sent to %s", s.done(action), p.Type, strings.Join(keys, ", "), endpoints)
		if len(remove) > 0 {
			msg += "; removes " + strings.Join(remove, ", ")
		}
		s.report("provider", p.Name, msg, true)
	}
}

// endpoints describes where credentials of a provider of type typ may be
// sent, according to its profile: the set's profile of that ID if it has
// one, else the gateway's. It reports whether a profile of that ID exists.
func (s *syncer) endpoints(typ string, profiles []openshell.Profile) (string, bool) {
	var eps []openshell.Endpoint
	if p, ok := s.set.Profile(typ); ok {
		eps = p.Endpoints
	} else {
		p, ok := effectiveProfile(profiles, typ)
		if !ok {
			return "", false
		}
		eps = p.Endpoints
	}
	if len(eps) == 0 {
		return "the endpoints that sandbox policies bind to it", true
	}
	s2 := make([]string, len(eps))
	for i, e := range eps {
		s2[i] = e.String()
	}
	return strings.Join(s2, ", "), true
}

// lookup fetches the provider's credentials and their fingerprints.
func (s *syncer) lookup(ctx context.Context, p Provider) (secrets, fingerprints map[string]string, err error) {
	secrets, fingerprints = map[string]string{}, map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(p.Credentials)) {
		if s.lookupErr != nil {
			return nil, nil, fmt.Errorf("credential %s: not looked up after an earlier lookup failed: %w", key, s.lookupErr)
		}
		ref := p.Credentials[key].SecretTool
		secret, err := s.secrets.Lookup(ctx, ref.Lookup)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			s.lookupErr = err
		}
		if err != nil {
			return nil, nil, fmt.Errorf("credential %s: %w", key, err)
		}
		fp, err := s.st.fingerprint(p.Name, key, secret)
		if err != nil {
			return nil, nil, err
		}
		secrets[key], fingerprints[key] = secret, fp
	}
	return secrets, fingerprints, nil
}

func (s *syncer) pruneProviders(ctx context.Context, providers []openshell.Provider) {
	for _, name := range slices.Sorted(maps.Keys(s.st.Providers)) {
		if slices.ContainsFunc(s.set.Providers, func(p Provider) bool { return p.Name == name }) {
			continue
		}
		ps := s.st.Providers[name]
		i := slices.IndexFunc(providers, func(p openshell.Provider) bool { return p.Name == name })
		switch {
		case !ps.Created:
			s.forget("provider", name, "no longer configured; left as it is, since brig did not create it", func() { delete(s.st.Providers, name) })
		// A record whose provider is gone or replaced is dropped on every sync,
		// not only with --prune: a provider created by hand later under the
		// same name must not count as brig's.
		case i < 0:
			s.forget("provider", name, "no longer configured and already gone", func() { delete(s.st.Providers, name) })
		case providers[i].Type != ps.Type:
			s.forget("provider", name, fmt.Sprintf("no longer configured; left as it is, since it has type %s now", providers[i].Type), func() { delete(s.st.Providers, name) })
		case !s.opts.Prune:
			s.report("provider", name, "no longer configured; kept (brig sync --prune deletes it)", true)
		default:
			if !s.opts.DryRun {
				if err := s.gw.DeleteProvider(ctx, name); err != nil {
					s.fail("provider", name, err)
					continue
				}
				delete(s.st.Providers, name)
			}
			s.report("provider", name, s.done("delete"), true)
		}
	}
}

func (s *syncer) pruneProfiles(ctx context.Context, profiles []openshell.Profile) {
	ws := workspaceProfiles(profiles)
	for _, id := range slices.Sorted(maps.Keys(s.st.Profiles)) {
		if _, ok := s.set.Profile(id); ok {
			continue
		}
		_, exists := ws[id]
		switch {
		case !s.st.Profiles[id].Created:
			s.forget("profile", id, "no longer configured; left as it is, since brig did not create it", func() { delete(s.st.Profiles, id) })
		case !exists:
			s.forget("profile", id, "no longer configured and already gone", func() { delete(s.st.Profiles, id) })
		case !s.opts.Prune:
			s.report("profile", id, "no longer configured; kept (brig sync --prune deletes it)", true)
		default:
			if !s.opts.DryRun {
				if err := s.gw.DeleteProfile(ctx, id); err != nil {
					s.fail("profile", id, err)
					continue
				}
				delete(s.st.Profiles, id)
			}
			s.report("profile", id, s.done("delete"), true)
		}
	}
}

// forget drops a profile or provider from the state, unless in a dry run.
func (s *syncer) forget(kind, name, msg string, drop func()) {
	if !s.opts.DryRun {
		drop()
	}
	s.report(kind, name, msg, true)
}
