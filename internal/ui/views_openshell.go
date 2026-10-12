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
	"github.com/dennisklein/brig/internal/ui/osclient"
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
// only, by VM and then name.
func sandboxRows(sn store.Snapshot, vm string) []sandboxRow {
	var out []sandboxRow
	for _, g := range sn.Gateways {
		if vm != "" && g.VM != vm {
			continue
		}
		for _, s := range g.Sandboxes {
			out = append(out, sandboxRow{VM: g.VM, Gateway: g, Sandbox: s})
		}
	}
	return out
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

// providerRows lists the gateway's providers with the config directories'
// providers that it lacks, by name.
func providerRows(g store.Gateway) []providerRow {
	var out []providerRow
	for _, p := range g.Providers {
		out = append(out, providerRow{Name: p.Name, Type: p.Type, Credentials: p.Credentials, Source: g.Config.Providers[p.Name]})
	}
	for name, path := range g.Config.Providers {
		if _, ok := g.Provider(name); !ok {
			out = append(out, providerRow{Name: name, Source: path, Missing: true})
		}
	}
	slices.SortFunc(out, func(a, b providerRow) int { return strings.Compare(a.Name, b.Name) })
	for i := range out {
		for _, s := range g.Sandboxes {
			if slices.Contains(s.Providers, out[i].Name) {
				out[i].Sandboxes = append(out[i].Sandboxes, s.Name)
			}
		}
	}
	return out
}

// profileRow is a provider profile with what uses it.
type profileRow struct {
	store.Profile
	// File is the file of the config directories that defines it, if any.
	File string
	// Providers are the gateway's providers of this type.
	Providers []string
}

// profileRows lists the gateway's provider profiles, those its providers
// use or its config directories define first.
func profileRows(g store.Gateway) []profileRow {
	out := make([]profileRow, 0, len(g.Profiles))
	for _, p := range g.Profiles {
		r := profileRow{Profile: p, File: g.Config.Profiles[p.ID]}
		for _, prov := range g.Providers {
			if prov.Type == p.ID {
				r.Providers = append(r.Providers, prov.Name)
			}
		}
		out = append(out, r)
	}
	used := func(r profileRow) bool { return len(r.Providers) > 0 || r.File != "" }
	slices.SortStableFunc(out, func(a, b profileRow) int {
		switch {
		case used(a) && !used(b):
			return -1
		case used(b) && !used(a):
			return 1
		}
		return 0
	})
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

// oscmd writes an openshell command for the gateway of the VM named vm, as
// a user would type it: the gateway named, since the user's shells may
// point at another one.
func oscmd(vm string, args ...string) string {
	return shellJoin(append([]string{"openshell", "-g", "brig-" + vm}, args...))
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
					Sev: "bad", Title: "Sandbox " + s.Name + " on " + g.VM + " failed", Detail: s.Problem,
					Link: "/sandboxes/" + g.VM + "/" + s.Name, Action: "Open",
				})
			}
		}
	}
	return out
}

// openshellFuncs returns the template functions of the OpenShell pages.
func openshellFuncs() template.FuncMap {
	return template.FuncMap{
		"sandboxRows":   sandboxRows,
		"providerRows":  providerRows,
		"profileRows":   profileRows,
		"policyLines":   policyLines,
		"phaseClass":    phaseClass,
		"gatewayClass":  gatewayClass,
		"otherMinor":    otherMinor,
		"oscmd":         oscmd,
		"stubsVersion":  func() string { return osclient.StubsVersion },
		"lower":         strings.ToLower,
		"fileSize":      fileSize,
		"readyGateways": readyGateways,
		"unattached":    unattached,
		"formHas":       func(f url.Values, key, v string) bool { return slices.Contains(f[key], v) },
		"sandboxTabs":   func() []tabDef { return sandboxTabs },
		"gatewayTabs":   func() []tabDef { return gatewayTabs },
		"gatewayOf": func(sn store.Snapshot, vm string) store.Gateway {
			g, _ := sn.Gateway(vm)
			return g
		},
		"providerOf": func(g store.Gateway, name string) *store.Provider {
			if p, ok := g.Provider(name); ok {
				return &p
			}
			return nil
		},
		"profileOf": func(g store.Gateway, id string) *store.Profile {
			if p, ok := g.Profile(id); ok {
				return &p
			}
			return nil
		},
		"templateSandboxes": func(g store.Gateway, name string) []string {
			var out []string
			for _, s := range g.Sandboxes {
				if s.Template == name {
					out = append(out, s.Name)
				}
			}
			return out
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
			items = append(items, paletteItem{"Sandbox " + s.Name, "on " + g.VM, "/sandboxes/" + g.VM + "/" + s.Name})
		}
	}
	return items
}
