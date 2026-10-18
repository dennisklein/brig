// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Workspace is the workspace the gateway works in.
	Workspace() string
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
	// HoldNewEndpoints keeps back profile changes that could change where or
	// how the credentials of providers on the gateway are sent: anything
	// but a profile's name, description and category. Only an explicit sync
	// then widens where credentials may be sent.
	HoldNewEndpoints bool
	// HoldNewProviders keeps back creating a provider, giving secrets to one
	// that brig did not create, and changing where a provider's credential
	// is looked up, once the gateway holds credentials that brig set: a new
	// pairing of a secret with a profile could send a secret that the
	// gateway already holds to other endpoints. Only an explicit sync then
	// makes such pairings. The first sync of a gateway is not held back.
	HoldNewProviders bool
	// Verbose also reports what is up to date.
	Verbose bool
	// Workspaces limits SyncAll to these workspaces; empty means all.
	Workspaces []string
	// Providers limits a sync to the providers of these names and to the
	// profiles they are instances of, so that changing one secret does not
	// also apply an unrelated pending change. Nothing is pruned then.
	Providers []string
	// Out receives one line per profile or provider.
	Out io.Writer
	// Record, if set, also receives each line as a Change, up-to-date
	// ones included.
	Record func(Change)
}

// Change is one line of a sync's report: what happened, or in a dry run
// would happen, to a workspace, profile or provider.
type Change struct {
	Workspace string `json:"workspace"`
	// Kind is "workspace", "profile" or "provider".
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Result says what was done, such as "imported" or "up to date"; it is
	// empty when Error says why it failed.
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// record passes c to o.Record, if set.
func (o Options) record(c Change) {
	if o.Record != nil {
		o.Record(c)
	}
}

// Host is a gateway as a whole: what SyncAll needs to reach each of its
// workspaces and to create missing ones. The CLI's workspace commands are
// the only place that need assumptions about how workspaces are created.
type Host interface {
	// Workspaces lists the names of the gateway's workspaces.
	Workspaces(ctx context.Context) ([]string, error)
	// CreateWorkspace creates a workspace.
	CreateWorkspace(ctx context.Context, name string) error
	// In returns the gateway as seen from one workspace.
	In(workspace string) (Gateway, error)
}

// SyncAll syncs every workspace of the set, the default workspace first,
// then the others by name, and joins the errors. A workspace that the state
// remembers but the set no longer defines is synced against an empty set, so
// that Prune can remove what brig created in it; the workspace itself stays.
// A workspace that the set defines and the gateway lacks is created and
// recorded as created by brig; brig never deletes a workspace. With
// opts.Workspaces only those workspaces are synced. When more than one
// workspace is synced, each one's report comes under a "workspace NAME:"
// heading, and its errors are prefixed likewise.
func SyncAll(ctx context.Context, set *Set, host Host, secrets Secrets, st *State, opts Options) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	// Decided once for all workspaces: creating the first workspace's
	// providers must not hold back the next one's.
	opts.HoldNewProviders = opts.HoldNewProviders && st.holdsCredentials()
	known := slices.Collect(maps.Keys(set.Workspaces))
	known = append(known, slices.Collect(maps.Keys(st.Workspaces))...)
	names := orderedWorkspaces(known)
	if len(opts.Workspaces) > 0 {
		var errs []error
		for _, name := range opts.Workspaces {
			if !slices.Contains(names, name) {
				errs = append(errs, fmt.Errorf("workspace %s is not defined by the config directories", name))
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
		names = slices.DeleteFunc(names, func(n string) bool { return !slices.Contains(opts.Workspaces, n) })
	}
	var (
		errs      []error
		onGateway []string
		listed    bool
		listErr   error
	)
	for _, name := range names {
		wopts := opts
		if len(names) > 1 {
			wopts.Out = &headingWriter{w: opts.Out, heading: "workspace " + name + ":\n"}
		}
		wrap := func(err error) {
			switch {
			case err == nil:
			case len(names) > 1:
				errs = append(errs, fmt.Errorf("workspace %s: %w", name, err))
			default:
				errs = append(errs, err)
			}
		}
		wst := st.Workspace(name)
		if name != openshell.DefaultWorkspace {
			if !listed {
				onGateway, listErr = host.Workspaces(ctx)
				listed = true
			}
			if listErr != nil {
				wrap(listErr)
				continue
			}
			if !slices.Contains(onGateway, name) {
				if _, defined := set.Workspaces[name]; !defined {
					// Deleted by hand: nothing left to prune.
					if !opts.DryRun {
						delete(st.Workspaces, name)
					}
					continue
				}
				if !opts.DryRun {
					if err := host.CreateWorkspace(ctx, name); err != nil {
						fmt.Fprintf(wopts.Out, "workspace %s: failed: %v\n", name, err)
						opts.record(Change{Workspace: name, Kind: "workspace", Name: name, Error: err.Error()})
						wrap(fmt.Errorf("create workspace: %w", err))
						continue
					}
					wst.Created = true
				}
				action := "created"
				if opts.DryRun {
					action = "would create"
				}
				fmt.Fprintf(wopts.Out, "workspace %s: %s\n", name, action)
				opts.record(Change{Workspace: name, Kind: "workspace", Name: name, Result: action})
				if opts.DryRun {
					continue
				}
			}
		}
		gw, err := host.In(name)
		if err != nil {
			wrap(err)
			continue
		}
		wrap(Sync(ctx, set.Workspaces[name], gw, secrets, st, wst, wopts))
	}
	return errors.Join(errs...)
}

// headingWriter writes heading before its first write.
type headingWriter struct {
	w       io.Writer
	heading string
	done    bool
}

func (h *headingWriter) Write(p []byte) (int, error) {
	if !h.done {
		h.done = true
		if _, err := io.WriteString(h.w, h.heading); err != nil {
			return 0, err
		}
	}
	return h.w.Write(p)
}

// Sync makes one workspace of the gateway gw, which works in that
// workspace, match set: it imports or updates the set's profiles, creates or
// updates its providers with credentials looked up through secrets, and with
// Prune deletes what brig created earlier but the set no longer has.
// Profiles and providers that brig did not create are only touched when the
// set defines them, and never deleted. Sync records what it did in ws, the
// workspace's part of st, and carries on after a failed step; it returns the
// errors of all failed steps.
func Sync(ctx context.Context, set WorkspaceSet, gw Gateway, secrets Secrets, st *State, ws *WorkspaceState, opts Options) error {
	if ws.Profiles == nil {
		ws.Profiles = map[string]ProfileState{}
	}
	if ws.Providers == nil {
		ws.Providers = map[string]ProviderState{}
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if len(opts.Providers) > 0 {
		set, opts.Prune = onlyProviders(set, opts.Providers), false
	}
	opts.HoldNewProviders = opts.HoldNewProviders && st.holdsCredentials()
	s := &syncer{set: set, gw: gw, secrets: secrets, st: st, ws: ws, opts: opts, unapplied: map[string]bool{}}
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
	if len(opts.Providers) == 0 {
		// Providers first: a profile cannot go while a provider uses it.
		s.pruneProviders(ctx, providers)
		s.pruneProfiles(ctx, profiles)
	}
	return errors.Join(s.errs...)
}

// onlyProviders returns the part of set that Options.Providers names: those
// providers and the profiles of their types.
func onlyProviders(set WorkspaceSet, names []string) WorkspaceSet {
	var out WorkspaceSet
	types := map[string]bool{}
	for _, p := range set.Providers {
		if slices.Contains(names, p.Name) {
			out.Providers = append(out.Providers, p)
			types[p.Type] = true
		}
	}
	for _, p := range set.Profiles {
		if types[p.ID] {
			out.Profiles = append(out.Profiles, p)
		}
	}
	return out
}

type syncer struct {
	set     WorkspaceSet
	gw      Gateway
	secrets Secrets
	st      *State
	ws      *WorkspaceState
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
	s.opts.record(Change{Workspace: s.gw.Workspace(), Kind: kind, Name: name, Result: msg})
}

func (s *syncer) fail(kind, name string, err error) {
	fmt.Fprintf(s.opts.Out, "%s %s: failed: %v\n", kind, name, err)
	s.opts.record(Change{Workspace: s.gw.Workspace(), Kind: kind, Name: name, Error: err.Error()})
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
// workspace uses: its own, else the platform's.
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

// cosmeticKeys are the top-level keys of a profile that do not decide where
// or how its credentials are sent. Every other key does, unknown ones too.
var cosmeticKeys = []string{"id", "display_name", "description", "category", "resource_version"}

// maxRoutingBytes bounds the canonical form that routing builds, which
// aliases can make much larger than the file.
const maxRoutingBytes = 16 << 20

// routing returns a digest of each top-level key of the profile that decides
// where or how its credentials are sent, such as credentials, endpoints and
// binaries. The digests are of the values as written, with aliases resolved:
// two spellings that a YAML parser may read alike, such as 443 and 0673,
// differ.
func routing(p ProfileFile) (map[string]string, error) {
	src, err := yamlSource(p.Path, p.Data)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", p.Path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a YAML mapping", p.Path)
	}
	m := doc.Content[0]
	budget := maxRoutingBytes
	out := map[string]string{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := resolveAlias(m.Content[i])
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s: a top-level key is not a string", p.Path)
		}
		if slices.Contains(cosmeticKeys, k.Value) {
			continue
		}
		val, err := canonical(m.Content[i+1], &budget)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Path, err)
		}
		if _, dup := out[k.Value]; dup {
			return nil, fmt.Errorf("%s: key %s is given twice", p.Path, k.Value)
		}
		sum := sha256.Sum256([]byte(val))
		out[k.Value] = hex.EncodeToString(sum[:])
	}
	return out, nil
}

// canonical writes the YAML node n in a form that differs for any two values
// that are written differently, apart from layout, comments, anchors and the
// order of mapping keys. budget bounds the bytes it writes.
func canonical(n *yaml.Node, budget *int) (string, error) {
	n = resolveAlias(n)
	var b strings.Builder
	switch n.Kind {
	case yaml.ScalarNode:
		fmt.Fprintf(&b, "s%d:%s%d:%s", len(n.ShortTag()), n.ShortTag(), len(n.Value), n.Value)
	case yaml.SequenceNode:
		b.WriteString("[")
		for _, c := range n.Content {
			s, err := canonical(c, budget)
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		}
		b.WriteString("]")
	case yaml.MappingNode:
		var entries []string
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, err := canonical(n.Content[i], budget)
			if err != nil {
				return "", err
			}
			v, err := canonical(n.Content[i+1], budget)
			if err != nil {
				return "", err
			}
			entries = append(entries, k+v)
		}
		slices.Sort(entries)
		fmt.Fprintf(&b, "{%s}", strings.Join(entries, ""))
	default:
		return "", fmt.Errorf("unexpected YAML node kind %v", n.Kind)
	}
	if *budget -= b.Len(); *budget < 0 {
		return "", errors.New("the profile is too large once its aliases are resolved")
	}
	return b.String(), nil
}

// changedKeys returns the keys whose digests differ between cur and next,
// sorted, those that only one of them has included.
func changedKeys(cur, next map[string]string) []string {
	var changed []string
	for k, v := range next {
		if cur[k] != v {
			changed = append(changed, k)
		}
	}
	for k := range cur {
		if _, ok := next[k]; !ok {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}

// routingChange says how a profile change could change where and how the
// credentials of the providers users are sent, which use the profile.
type routingChange struct {
	users []string
	// added are endpoints that the gateway's profile lacks.
	added []string
	// changed are the top-level keys that differ from the profile that
	// brig applied, apart from cosmetic ones.
	changed []string
	// unknown is set when brig has no record of the gateway's profile, so
	// that changed is unknown.
	unknown bool
}

func (c routingChange) any() bool {
	return len(c.users) > 0 && (len(c.added) > 0 || len(c.changed) > 0 || c.unknown)
}

// reasons phrases the change for the report.
func (c routingChange) reasons(added, changed, unknown string) []string {
	users := strings.Join(c.users, ", ")
	var why []string
	if len(c.added) > 0 {
		why = append(why, fmt.Sprintf(added, users, strings.Join(c.added, ", ")))
	}
	// New endpoints are named already.
	keys := c.changed
	if len(c.added) > 0 {
		keys = slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k == "endpoints" })
	}
	if len(keys) > 0 {
		why = append(why, fmt.Sprintf(changed, strings.Join(keys, ", "), users))
	}
	if c.unknown {
		why = append(why, fmt.Sprintf(unknown, users))
	}
	return why
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
		prev, managed := s.ws.Profiles[p.ID]
		route, err := routing(p)
		if err != nil {
			s.unapplied[p.ID] = true
			s.fail("profile", p.ID, err)
			continue
		}
		if exists && managed && prev.SHA256 == p.SHA256() {
			// A state from before brig recorded routing: the file is what
			// the gateway has.
			if prev.Routing == nil && !s.opts.DryRun {
				prev.Routing = route
				s.ws.Profiles[p.ID] = prev
			}
			s.report("profile", p.ID, "up to date", false)
			continue
		}
		// Credentials that the gateway holds already must not silently
		// gain destinations or be sent in another way.
		var rc routingChange
		for _, pr := range providers {
			if pr.Type == p.ID {
				rc.users = append(rc.users, pr.Name)
			}
		}
		slices.Sort(rc.users)
		if len(rc.users) > 0 {
			eff, _ := effectiveProfile(profiles, p.ID)
			rc.added = addedEndpoints(eff.Endpoints, p.Endpoints)
			if exists && managed && prev.Routing != nil {
				rc.changed = changedKeys(prev.Routing, route)
			} else {
				rc.unknown = true
			}
		}
		if rc.any() && s.opts.HoldNewEndpoints {
			s.unapplied[p.ID] = true
			why := rc.reasons("it would let the credentials of providers %s also go to %s",
				"it changes %s, which decide where and how the credentials of providers %s are sent",
				"brig has no record of the profile that the gateway uses for providers %s, so it cannot tell where their credentials would go")
			s.fail("profile", p.ID, fmt.Errorf("held back: %s; review with `brig sync --dry-run` and apply with `brig sync`", strings.Join(why, "; ")))
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
				s.ws.Profiles[p.ID] = ProfileState{Created: true}
			}
		}
		if err != nil {
			s.unapplied[p.ID] = true
			s.fail("profile", p.ID, err)
			continue
		}
		if !s.opts.DryRun {
			s.ws.Profiles[p.ID] = ProfileState{SHA256: p.SHA256(), Created: !exists || (managed && prev.Created), Routing: route}
		}
		msg := s.done(action) + " from " + p.Path
		if rc.any() {
			msg += "; " + strings.Join(rc.reasons("the credentials of providers %s may now also go to %s",
				"it changes %s, which decide where and how the credentials of providers %s are sent",
				"brig has no record of the profile that the gateway used for providers %s before"), "; ")
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
			s.fail("provider", p.Name, fmt.Errorf("the gateway has a provider of this name with type %s, not %s; delete it first with `openshell -g %s --workspace %s provider delete %s`", cur.Type, p.Type, s.gw.Name(), s.gw.Workspace(), p.Name))
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
		prev, managed := s.ws.Providers[p.Name]
		sources := p.sources()
		// Decided before any lookup, so that a held-back provider's
		// secrets are not even read.
		if why := s.newPairing(exists, managed, prev, sources); why != "" {
			s.fail("provider", p.Name, fmt.Errorf("held back: %s; review with `brig sync --dry-run` and apply with `brig sync`", why))
			continue
		}
		secrets, fingerprints, err := s.lookup(ctx, p)
		if err != nil {
			s.fail("provider", p.Name, err)
			continue
		}
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
		upToDate := exists && managed && len(remove) == 0 && !missing && maps.Equal(prev.Credentials, fingerprints)
		if upToDate && prev.Sources == nil && !s.opts.DryRun {
			// A state from before brig recorded sources: these give the
			// secrets that the gateway holds.
			prev.Sources = sources
			s.ws.Providers[p.Name] = prev
		}
		if upToDate && !s.opts.RefreshSecrets {
			s.report("provider", p.Name, "up to date", false)
			continue
		}
		if s.opts.HoldNewProviders && prev.Sources == nil {
			s.fail("provider", p.Name, errors.New("held back: brig has no record of where its credentials were looked up; review with `brig sync --dry-run` and apply with `brig sync`"))
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
				s.ws.Providers[p.Name] = ProviderState{Type: p.Type, Created: true, Sources: sources}
			}
			if err != nil {
				s.fail("provider", p.Name, err)
				continue
			}
			s.ws.Providers[p.Name] = ProviderState{Type: p.Type, Credentials: fingerprints, Created: !exists || (managed && prev.Created), Sources: sources}
		}
		keys := slices.Sorted(maps.Keys(p.Credentials))
		msg := fmt.Sprintf("%s (type %s); %s may be sent to %s", s.done(action), p.Type, strings.Join(keys, ", "), endpoints)
		if len(remove) > 0 {
			msg += "; removes " + strings.Join(remove, ", ")
		}
		s.report("provider", p.Name, msg, true)
	}
}

// newPairing says why HoldNewProviders keeps a provider back, if it does:
// the provider would pair secrets with a profile in a way that no explicit
// sync made. exists and managed say whether the gateway has it and whether
// brig created or set it, prev is its state and sources are its credentials'
// sources now.
func (s *syncer) newPairing(exists, managed bool, prev ProviderState, sources map[string]string) string {
	switch {
	case !s.opts.HoldNewProviders:
		return ""
	// A provider that brig set before, even if its create failed or it was
	// deleted since, is recreated with the sources it had.
	case !exists && !managed:
		return "brig start creates no provider once the gateway holds credentials, since a new one could send them to other endpoints"
	case !managed:
		return "brig start gives no secrets to a provider that brig did not create"
	case prev.Sources == nil:
		// Told apart once the secrets are looked up: an up-to-date
		// provider is recorded as it is.
		return ""
	}
	var moved []string
	for _, key := range slices.Sorted(maps.Keys(sources)) {
		if prev.Sources[key] != sources[key] {
			moved = append(moved, key)
		}
	}
	if len(moved) == 0 {
		return ""
	}
	return fmt.Sprintf("it would look %s up elsewhere than brig did before", strings.Join(moved, ", "))
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
		var secret string
		var err error
		if c := p.Credentials[key]; c.Env != "" {
			secret, err = LookupEnv(c.Env)
		} else {
			secret, err = s.secrets.Lookup(ctx, c.SecretTool.Lookup)
		}
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
	for _, name := range slices.Sorted(maps.Keys(s.ws.Providers)) {
		if slices.ContainsFunc(s.set.Providers, func(p Provider) bool { return p.Name == name }) {
			continue
		}
		ps := s.ws.Providers[name]
		i := slices.IndexFunc(providers, func(p openshell.Provider) bool { return p.Name == name })
		switch {
		case !ps.Created:
			s.forget("provider", name, "no longer configured; left as it is, since brig did not create it", func() { delete(s.ws.Providers, name) })
		// A record whose provider is gone or replaced is dropped on every sync,
		// not only with --prune: a provider created by hand later under the
		// same name must not count as brig's.
		case i < 0:
			s.forget("provider", name, "no longer configured and already gone", func() { delete(s.ws.Providers, name) })
		case providers[i].Type != ps.Type:
			s.forget("provider", name, fmt.Sprintf("no longer configured; left as it is, since it has type %s now", providers[i].Type), func() { delete(s.ws.Providers, name) })
		case !s.opts.Prune:
			s.report("provider", name, "no longer configured; kept (brig sync --prune deletes it)", true)
		default:
			if !s.opts.DryRun {
				if err := s.gw.DeleteProvider(ctx, name); err != nil {
					s.fail("provider", name, err)
					continue
				}
				delete(s.ws.Providers, name)
			}
			s.report("provider", name, s.done("delete"), true)
		}
	}
}

func (s *syncer) pruneProfiles(ctx context.Context, profiles []openshell.Profile) {
	ws := workspaceProfiles(profiles)
	for _, id := range slices.Sorted(maps.Keys(s.ws.Profiles)) {
		if _, ok := s.set.Profile(id); ok {
			continue
		}
		_, exists := ws[id]
		switch {
		case !s.ws.Profiles[id].Created:
			s.forget("profile", id, "no longer configured; left as it is, since brig did not create it", func() { delete(s.ws.Profiles, id) })
		case !exists:
			s.forget("profile", id, "no longer configured and already gone", func() { delete(s.ws.Profiles, id) })
		case !s.opts.Prune:
			s.report("profile", id, "no longer configured; kept (brig sync --prune deletes it)", true)
		default:
			if !s.opts.DryRun {
				if err := s.gw.DeleteProfile(ctx, id); err != nil {
					s.fail("profile", id, err)
					continue
				}
				delete(s.ws.Profiles, id)
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
