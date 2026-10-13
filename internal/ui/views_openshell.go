// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
)

// sandboxTabs are the tabs of a sandbox's page, in order.
var sandboxTabs = []tabDef{
	{"overview", "Overview"},
	{"policy", "Policy"},
	{"traffic", "Traffic"},
	{"files", "Files"},
}

// gatewayTabs are the tabs of a gateway's page, in order.
var gatewayTabs = []tabDef{
	{"providers", "Providers"},
	{"profiles", "Provider profiles"},
	{"policies", "Policies"},
	{"templates", "Templates"},
}

// tabDef is a tab of a page: its key in the URL and its label.
type tabDef struct{ Key, Label string }

// pickTab returns the tab of tabs named key, or the first.
func pickTab(tabs []tabDef, key string) string {
	for _, t := range tabs {
		if t.Key == key {
			return key
		}
	}
	return tabs[0].Key
}

// sandboxRow is a sandbox in the list of all VMs' sandboxes.
type sandboxRow struct {
	VM      string
	Gateway store.Gateway
	store.Sandbox
}

// sandboxRows lists the sandboxes of every gateway, or of the VM named vm
// and the workspace ws only, by VM, workspace and name.
func sandboxRows(sn store.Snapshot, vm, ws string) []sandboxRow {
	var out []sandboxRow
	for _, g := range sn.Gateways {
		if vm != "" && g.VM != vm {
			continue
		}
		for _, s := range g.Sandboxes {
			if ws != "" && s.Workspace != ws {
				continue
			}
			out = append(out, sandboxRow{VM: g.VM, Gateway: g, Sandbox: s})
		}
	}
	return out
}

// workspacesOf lists the workspaces whose objects a gateway's page groups:
// the gateway's, else the default one, so that a gateway that never answered
// still shows what its config directories define.
func workspacesOf(g store.Gateway) []string {
	if len(g.Workspaces) == 0 {
		return []string{store.DefaultWorkspace}
	}
	return g.Workspaces
}

// providerRow is a row of a gateway's providers: one on the gateway, one
// that its config directories define, or both.
type providerRow struct {
	Name, Type  string
	Credentials []string
	// Source is the file that defines it, empty if it was made by hand.
	Source string
	// Missing says that the config directories define it but the gateway
	// lacks it: the last sync did not create it.
	Missing bool
	// Sandboxes are those it is attached to.
	Sandboxes []string
}

// providerGroups lists the gateway's providers by workspace, each with the
// config directories' providers that it lacks, by name.
func providerGroups(g store.Gateway) []group[providerRow] {
	var out []group[providerRow]
	for _, ws := range workspacesOf(g) {
		grp := group[providerRow]{Workspace: ws, Heading: g.MultiWorkspace()}
		for _, p := range g.Providers {
			if p.Workspace != ws {
				continue
			}
			r := providerRow{Name: p.Name, Type: p.Type, Credentials: p.Credentials}
			r.Source = g.Config.Workspaces[ws].Providers[p.Name]
			grp.Rows = append(grp.Rows, r)
		}
		for name, path := range g.Config.Workspaces[ws].Providers {
			if _, ok := g.Provider(ws, name); !ok {
				grp.Rows = append(grp.Rows, providerRow{Name: name, Source: path, Missing: true})
			}
		}
		slices.SortFunc(grp.Rows, func(a, b providerRow) int { return strings.Compare(a.Name, b.Name) })
		for i := range grp.Rows {
			for _, s := range g.Sandboxes {
				if s.Workspace == ws && slices.Contains(s.Providers, grp.Rows[i].Name) {
					grp.Rows[i].Sandboxes = append(grp.Rows[i].Sandboxes, s.Name)
				}
			}
		}
		out = append(out, grp)
	}
	return out
}

// profileRow is a provider profile with what uses it.
type profileRow struct {
	store.Profile
	// File is the file of the config directories that defines it, if any.
	File string
	// Providers are the gateway's providers of this type, with their
	// workspace if the gateway has several.
	Providers []string
}

// profileGroups lists the gateway's provider profiles by workspace, those
// its providers use or its config directories define first. With several
// workspaces the profiles of the platform, which every workspace sees, come
// first as a group of their own; with one they join its group.
func profileGroups(g store.Gateway) []group[profileRow] {
	multi := g.MultiWorkspace()
	row := func(p store.Profile) profileRow {
		r := profileRow{Profile: p}
		r.File = g.Config.Workspaces[p.Workspace].Profiles[p.ID]
		for _, prov := range g.Providers {
			if prov.Type != p.ID || (p.Workspace != "" && prov.Workspace != p.Workspace) {
				continue
			}
			name := prov.Name
			if multi && p.Workspace == "" {
				name += " (" + prov.Workspace + ")"
			}
			r.Providers = append(r.Providers, name)
		}
		return r
	}
	used := func(r profileRow) bool { return len(r.Providers) > 0 || r.File != "" }
	sorted := func(rows []profileRow) []profileRow {
		slices.SortStableFunc(rows, func(a, b profileRow) int {
			switch {
			case used(a) && !used(b):
				return -1
			case used(b) && !used(a):
				return 1
			}
			return 0
		})
		return rows
	}
	if !multi {
		grp := group[profileRow]{Workspace: workspacesOf(g)[0]}
		for _, p := range g.Profiles {
			grp.Rows = append(grp.Rows, row(p))
		}
		grp.Rows = sorted(grp.Rows)
		if len(grp.Rows) == 0 {
			return nil
		}
		return []group[profileRow]{grp}
	}
	var out []group[profileRow]
	for _, ws := range append([]string{""}, g.Workspaces...) {
		grp := group[profileRow]{Workspace: ws, Heading: true}
		for _, p := range g.Profiles {
			if p.Workspace == ws {
				grp.Rows = append(grp.Rows, row(p))
			}
		}
		if ws == "" && len(grp.Rows) == 0 {
			continue
		}
		grp.Rows = sorted(grp.Rows)
		out = append(out, grp)
	}
	return out
}

// templateRow is a sandbox template with the sandboxes made from it.
type templateRow struct {
	store.Template
	Sandboxes []string
}

// templateGroups lists the gateway's sandbox templates by workspace.
func templateGroups(g store.Gateway) []group[templateRow] {
	var out []group[templateRow]
	for _, ws := range workspacesOf(g) {
		grp := group[templateRow]{Workspace: ws, Heading: g.MultiWorkspace()}
		for _, t := range g.Templates {
			if t.Workspace != ws {
				continue
			}
			r := templateRow{Template: t}
			for _, s := range g.Sandboxes {
				if s.Workspace == ws && s.Template == t.Name {
					r.Sandboxes = append(r.Sandboxes, s.Name)
				}
			}
			grp.Rows = append(grp.Rows, r)
		}
		out = append(out, grp)
	}
	return out
}

// policyLine is a line of a policy as a page shows it.
type policyLine struct {
	Text string
	// Key is the mapping key that starts the line, if any, and Rest the
	// text after it.
	Indent, Key, Rest string
	// Provider names the attached provider whose rule the line belongs to.
	Provider string
	// First marks the first line of a provider's rule.
	First bool
}

var keyLineRE = regexp.MustCompile(`^(\s*(?:- )?)([A-Za-z0-9_.-]+):(.*)$`)

// policyLines splits a policy into lines and marks the lines of the rules
// that the attached providers add, which brig ui writes two spaces deep
// under network_policies.
func policyLines(text string, rules []string, providers []string) []policyLine {
	byRule := map[string]string{}
	for _, p := range providers {
		byRule[providerRuleName(p)] = p
	}
	var out []policyLine
	inRule := ""
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		l := policyLine{Text: line}
		if m := keyLineRE.FindStringSubmatch(line); m != nil {
			l.Indent, l.Key, l.Rest = m[1], m[2], m[3]
		}
		depth := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case depth == 2 && l.Key != "" && slices.Contains(rules, l.Key):
			inRule = l.Key
			l.First = true
		case depth <= 2:
			inRule = ""
		}
		if inRule != "" {
			l.Provider = byRule[inRule]
			if l.Provider == "" {
				l.Provider = strings.TrimPrefix(inRule, "_provider_")
			}
		}
		out = append(out, l)
	}
	return out
}

// providerRuleName is the key of the rule that attaching the provider name
// adds, as OpenShell's provider_rule_name makes it.
func providerRuleName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 'a' - 'A')
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		s = "unnamed"
	}
	return "_provider_" + s
}

// phaseClass is the colour of a sandbox phase's pill.
func phaseClass(phase string) string {
	switch phase {
	case "Ready":
		return "ok"
	case "Provisioning", "Starting", "Stopping", "Deleting":
		return "info"
	case "Error", "Unknown":
		return "bad"
	}
	return ""
}

// gatewayClass is the colour of a gateway state's pill.
func gatewayClass(state string) string {
	switch state {
	case store.GatewayReady:
		return "ok"
	case store.GatewayConnecting:
		return "info"
	case store.GatewayUnreachable:
		return "bad"
	}
	return ""
}

// otherMinor reports whether the gateway runs another minor version of
// OpenShell than the one whose stubs brig ui reads it with.
func otherMinor(g store.Gateway) bool {
	return g.Version != "" && !openshell.CompatibleVersions(osclient.StubsVersion, g.Version)
}

// oscmd writes an openshell command for the gateway of the VM named vm and
// its workspace ws, as a user would type it: both named, since the user's
// shells may point at another gateway or workspace.
func oscmd(vm, ws string, args ...string) string {
	return shellJoin(append([]string{"openshell", "-g", "brig-" + vm, "--workspace", ws}, args...))
}

// shellJoin writes argv for a POSIX shell, quoting the words that need it.
func shellJoin(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:=@%+,") == "" {
			words[i] = a
		} else {
			words[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}

// firstGateway is the gateway that the Gateway page shows when none is
// named: the first that answers, else the first.
func firstGateway(sn store.Snapshot) (store.Gateway, bool) {
	for _, g := range sn.Gateways {
		if g.State == store.GatewayReady {
			return g, true
		}
	}
	if len(sn.Gateways) > 0 {
		return sn.Gateways[0], true
	}
	return store.Gateway{}, false
}

// gatewayGrace is how long after its VM booted a gateway that does not
// answer is not worth a word: it may still be starting.
const gatewayGrace = 2 * time.Minute

// gatewayNeeds lists what the gateways need the user for.
func gatewayNeeds(sn store.Snapshot) []need {
	var out []need
	for _, g := range sn.Gateways {
		link := "/gateways/" + g.VM
		switch {
		case g.State == store.GatewayUnreachable:
			if v, ok := sn.VM(g.VM); ok && v.Running() && !v.Up.IsZero() && time.Since(v.Up) < gatewayGrace {
				// It may still be starting.
				continue
			}
			out = append(out, need{Sev: "bad", Title: "brig ui cannot reach " + g.Name(), Detail: g.Err, Link: link, Action: "Open"})
		case otherMinor(g):
			out = append(out, need{
				Sev: "warn", Title: g.Name() + " runs OpenShell " + g.Version,
				Detail: "brig ui reads gateways as OpenShell " + osclient.StubsVersion + " does; some details may be missing.",
				Link:   link, Action: "Open",
			})
		}
		for _, s := range g.Sandboxes {
			if s.Phase == "Error" {
				out = append(out, need{
					Sev: "bad", Title: "Sandbox " + sandboxLabel(sn, g.VM, s.Workspace, s.Name) + " on " + g.VM + " failed", Detail: s.Problem,
					Link: sandboxURL(g.VM, s.Workspace, s.Name), Action: "Open",
				})
			}
		}
	}
	return out
}

// openshellFuncs returns the template functions of the OpenShell pages.
func openshellFuncs() template.FuncMap {
	return template.FuncMap{
		"sandboxRows":    sandboxRows,
		"providerGroups": providerGroups,
		"profileGroups":  profileGroups,
		"templateGroups": templateGroups,
		"policyLines":    policyLines,
		"phaseClass":     phaseClass,
		"gatewayClass":   gatewayClass,
		"otherMinor":     otherMinor,
		"oscmd":          oscmd,
		"stubsVersion":   func() string { return osclient.StubsVersion },
		"lower":          strings.ToLower,
		"fileSize":       fileSize,
		"readyGateways":  readyGateways,
		"unattached":     unattached,
		"formHas":        func(f url.Values, key, v string) bool { return slices.Contains(f[key], v) },
		"sandboxTabs":    func() []tabDef { return sandboxTabs },
		"gatewayTabs":    func() []tabDef { return gatewayTabs },
		"gatewayOf": func(sn store.Snapshot, vm string) store.Gateway {
			g, _ := sn.Gateway(vm)
			return g
		},
		"providerOf": func(g store.Gateway, ws, name string) *store.Provider {
			if p, ok := g.Provider(ws, name); ok {
				return &p
			}
			return nil
		},
		"profileOf": func(g store.Gateway, ws, id string) *store.Profile {
			if p, ok := g.Profile(ws, id); ok {
				return &p
			}
			return nil
		},
		"providersIn": func(g store.Gateway, ws string) []store.Provider {
			return slices.DeleteFunc(slices.Clone(g.Providers), func(p store.Provider) bool { return p.Workspace != ws })
		},
		"templatesIn": func(g store.Gateway, ws string) []store.Template {
			return slices.DeleteFunc(slices.Clone(g.Templates), func(t store.Template) bool { return t.Workspace != ws })
		},
		"sandboxCount": func(sn store.Snapshot) string {
			ready, total := sn.SandboxCount()
			return fmt.Sprintf("%d/%d", ready, total)
		},
	}
}

// openshellPalette lists the jump box's entries for sandboxes and gateways.
func openshellPalette(sn store.Snapshot) []paletteItem {
	var items []paletteItem
	for _, g := range sn.Gateways {
		items = append(items, paletteItem{"Gateway " + g.Name(), g.State, "/gateways/" + g.VM})
		for _, t := range gatewayTabs[1:] {
			items = append(items, paletteItem{g.Name() + " · " + t.Label, "gateway", "/gateways/" + g.VM + "?tab=" + t.Key})
		}
		for _, s := range g.Sandboxes {
			on := "on " + g.VM
			if g.MultiWorkspace() {
				on += " · " + s.Workspace
			}
			items = append(items, paletteItem{"Sandbox " + s.Name, on, sandboxURL(g.VM, s.Workspace, s.Name)})
		}
	}
	return items
}
