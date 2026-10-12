// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"html/template"
	"net/url"
	"slices"

	"github.com/dennisklein/brig/internal/ui/store"
)

// A sandbox is identified by its VM, its workspace and its name: names are
// unique per workspace only. Pages say which workspace an object belongs to
// where it can be told apart, and always on a sandbox's own page and
// dialogs.

// sandboxURL is the path of a sandbox's page.
func sandboxURL(vm, workspace, name string) string {
	return "/sandboxes/" + vm + "/" + workspace + "/" + name
}

// sandboxTabURL is the link to a tab of a sandbox's page.
func sandboxTabURL(vm, workspace, name, tab string) string {
	return sandboxURL(vm, workspace, name) + "?tab=" + url.QueryEscape(tab)
}

// sandboxesURL is the Sandboxes page filtered by VM and workspace, either
// of which may be empty.
func sandboxesURL(vm, workspace string) string {
	q := url.Values{}
	if vm != "" {
		q.Set("vm", vm)
	}
	if workspace != "" {
		q.Set("workspace", workspace)
	}
	if len(q) == 0 {
		return "/sandboxes"
	}
	return "/sandboxes?" + q.Encode()
}

// multiWorkspace reports whether the gateway of the VM named vm has more
// than one workspace, so that rows must name theirs.
func multiWorkspace(sn store.Snapshot, vm string) bool {
	g, _ := sn.Gateway(vm)
	return g.MultiWorkspace()
}

// workspaceChoices lists the workspaces to filter by: those of the gateway of
// the VM named vm, or of all gateways when empty. It is empty when no
// gateway in scope has more than one workspace, since then there is nothing
// to choose.
func workspaceChoices(sn store.Snapshot, vm string) []string {
	var out []string
	multi := false
	for _, g := range sn.Gateways {
		if vm != "" && g.VM != vm {
			continue
		}
		multi = multi || g.MultiWorkspace()
		for _, w := range g.Workspaces {
			if !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	if !multi {
		return nil
	}
	slices.Sort(out)
	return out
}

// sandboxLabel names a sandbox in a sentence: its name, and its workspace
// when the gateway has several.
func sandboxLabel(sn store.Snapshot, vm, workspace, name string) string {
	if multiWorkspace(sn, vm) {
		return name + " (" + workspace + ")"
	}
	return name
}

// group is the objects of one workspace on a gateway's page. Heading says
// whether to show the workspace's name, which is so when the gateway has more
// than one. Workspace is empty for the objects of the platform.
type group[T any] struct {
	Workspace string
	Heading   bool
	Rows      []T
}

// sandboxFilterQuery is the Sandboxes page's filters as live-update
// parameters.
func sandboxFilterQuery(vm, workspace string) url.Values {
	return url.Values{"filter": {vm}, "wsfilter": {workspace}}
}

// workspaceFuncs returns the template functions for workspaces.
func workspaceFuncs() template.FuncMap {
	return template.FuncMap{
		"sandboxURL":       sandboxURL,
		"sandboxTabURL":    sandboxTabURL,
		"sandboxesURL":     sandboxesURL,
		"multiWorkspace":   multiWorkspace,
		"workspaceChoices": workspaceChoices,
		"sandboxLabel":     sandboxLabel,
		"workspacesOf":     workspacesOf,
		"inWorkspace": func(g store.Gateway, ws string) string {
			if g.MultiWorkspace() {
				return ", workspace " + ws
			}
			return ""
		},
	}
}
