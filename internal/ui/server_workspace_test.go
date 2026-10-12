// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

// The demo's dev gateway has two workspaces, with a sandbox called webapp in
// each; review has one.

func TestWorkspaceBadges(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	const badge = `title="Workspace"`
	body := ts.get("/sandboxes").Body.String()
	for _, want := range []string{
		`href="/sandboxes/dev/default/webapp"`, `href="/sandboxes/dev/team/webapp"`,
		`<span class="chip" title="Workspace">team</span>`, `aria-label="Filter by workspace"`, `href="/sandboxes?workspace=team"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/sandboxes lacks %q", want)
		}
	}
	// A gateway with one workspace needs no badge.
	if body := ts.get("/sandboxes?vm=review").Body.String(); strings.Contains(body, badge) || strings.Contains(body, "Filter by workspace") {
		t.Errorf("a gateway with one workspace shows its workspace:\n%s", body)
	}
	if body := ts.get("/vms/review?tab=sandboxes").Body.String(); strings.Contains(body, badge) {
		t.Error("the VM tab of a gateway with one workspace shows its workspace")
	}
	if body := ts.get("/vms/dev?tab=sandboxes").Body.String(); !strings.Contains(body, `href="/sandboxes/dev/team/webapp"`) || !strings.Contains(body, badge) {
		t.Errorf("the VM tab of dev does not tell the sandboxes apart:\n%s", body)
	}
	// A sandbox's own page always names the workspace.
	for target, ws := range map[string]string{
		"/sandboxes/review/default/pr-review": "default", "/sandboxes/dev/default/webapp": "default", "/sandboxes/dev/team/webapp": "team",
	} {
		body := ts.get(target).Body.String()
		if !strings.Contains(body, `workspace <span class="chip">`+ws+`</span>`) || !strings.Contains(body, "--workspace "+ws+" sandbox connect webapp") && ws != "default" {
			t.Errorf("%s does not name its workspace %s", target, ws)
		}
	}
}

func TestWorkspaceFilter(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	body := ts.get("/sandboxes?workspace=team").Body.String()
	if !strings.Contains(body, `href="/sandboxes/dev/team/webapp"`) || strings.Contains(body, "kit-dev") || strings.Contains(body, `href="/sandboxes/dev/default/webapp"`) {
		t.Errorf("the workspace filter shows other sandboxes:\n%s", body)
	}
	if body := ts.get("/sandboxes?workspace=nope").Body.String(); !strings.Contains(body, "in workspace nope") {
		t.Error("an empty workspace is not said to be empty")
	}
	// The VM filter keeps the workspace one.
	if body := ts.get("/sandboxes?workspace=team").Body.String(); !strings.Contains(body, `href="/sandboxes?vm=dev&amp;workspace=team"`) {
		t.Error("the VM filter drops the workspace filter")
	}
}

func TestWorkspaceSandboxPages(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	// The two webapps are different sandboxes.
	def := ts.get("/sandboxes/dev/default/webapp").Body.String()
	team := ts.get("/sandboxes/dev/team/webapp").Body.String()
	if !strings.Contains(def, "GH_TOKEN") || strings.Contains(team, "GH_TOKEN") {
		t.Error("the sandboxes of one name share providers")
	}
	if !strings.Contains(team, "/a/sandbox-delete?sandbox=webapp&amp;vm=dev&amp;workspace=team") || !strings.Contains(team, "/terminal?vm=dev&amp;workspace=team&amp;sandbox=webapp") {
		t.Errorf("the actions of team's webapp do not name its workspace:\n%s", team)
	}
	if body := ts.get("/sandboxes/dev/team/webapp?tab=policy").Body.String(); !strings.Contains(body, "openshell -g brig-dev --workspace team policy get webapp --base") || !strings.Contains(body, "brig policy diff dev webapp --workspace team") {
		t.Errorf("policy tab:\n%s", body)
	}
	if body := ts.get("/sandboxes/dev/team/webapp?tab=files").Body.String(); !strings.Contains(body, "--workspace team sandbox download webapp /sandbox/PATH .") || !strings.Contains(body, `hx-get="/sandboxes/dev/team/webapp/files?path=%2Fsandbox"`) {
		t.Errorf("files tab:\n%s", body)
	}
	// An unknown workspace, a sandbox of another workspace and the old
	// route are not found.
	for _, target := range []string{
		"/sandboxes/dev/nope/webapp", "/sandboxes/dev/team/kit-dev", "/sandboxes/review/team/pr-review", "/sandboxes/dev/webapp",
		"/sandboxes/dev/team/webapp/files?path=/sandbox/x/../..", "/sandboxes/dev/..%2f/webapp",
	} {
		if w := ts.get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, w.Code)
		}
	}
}

func TestWorkspaceGatewayPage(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for target, want := range map[string][]string{
		"/gateways/dev":                 {"workspaces default, team", `Workspace <span class="mono">team</span>`, `href="/sandboxes/dev/team/webapp"`, "--workspace team provider list"},
		"/gateways/dev?tab=profiles":    {`Platform <span`, `Workspace <span class="mono">team</span>`, "--workspace team profile list"},
		"/gateways/dev?tab=templates":   {`Workspace <span class="mono">team</span>`, "8Gi", "--workspace team sandbox template list"},
		"/gateways/dev?tab=policies":    {`<span class="chip" title="Workspace">team</span>`, "/sandboxes/dev/team/webapp?tab=policy"},
		"/gateways/review":              {"workspace default"},
		"/gateways/review?tab=profiles": {"--workspace default profile list"},
	} {
		body := ts.get(target).Body.String()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s lacks %q", target, s)
			}
		}
	}
	// With one workspace, nothing is grouped.
	for _, target := range []string{"/gateways/review", "/gateways/review?tab=profiles", "/gateways/review?tab=templates"} {
		if body := ts.get(target).Body.String(); strings.Contains(body, `class="grouprow"`) || strings.Contains(body, `class="group"`) {
			t.Errorf("%s groups by workspace", target)
		}
	}
}

func TestWorkspaceTrafficAndLinks(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	now := time.Now()
	ts.st.AddRequests("dev", []store.Request{
		{Workspace: "default", Sandbox: "webapp", Time: now.Add(-3 * time.Minute), Verdict: "deny", Process: "/usr/bin/node", Host: "default.example", Port: 443, Reason: "no rule"},
		{Workspace: "team", Sandbox: "webapp", Time: now.Add(-2 * time.Minute), Verdict: "deny", Process: "/usr/bin/node", Host: "team.example", Port: 443, Reason: "no rule"},
	})
	body := ts.get("/traffic").Body.String()
	for _, want := range []string{
		`href="/sandboxes/dev/team/webapp?tab=traffic"`, `href="/sandboxes/dev/default/webapp?tab=traffic"`, `<span class="chip" title="Workspace">team</span>`,
		`aria-label="Filter by workspace"`, "sandbox=webapp&amp;vm=dev&amp;workspace=default", "sandbox=webapp&amp;vm=dev&amp;workspace=team",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/traffic lacks %q", want)
		}
	}
	body = ts.get("/traffic?workspace=team").Body.String()
	if !strings.Contains(body, "team.example") || strings.Contains(body, "default.example") || !strings.Contains(body, "workspace team") {
		t.Errorf("/traffic?workspace=team:\n%s", body)
	}
	body = ts.get("/traffic?vm=dev&workspace=default&sandbox=webapp").Body.String()
	if !strings.Contains(body, "default.example") || strings.Contains(body, "team.example") {
		t.Errorf("/traffic of default's webapp:\n%s", body)
	}
	// The sandbox's own tab shows its requests only.
	body = ts.get("/sandboxes/dev/team/webapp?tab=traffic").Body.String()
	if !strings.Contains(body, "team.example") || strings.Contains(body, "default.example") {
		t.Errorf("team's Traffic tab:\n%s", body)
	}
	// Needs you names the workspace, and the Allow… button carries it.
	body = ts.get("/").Body.String()
	if !strings.Contains(body, "1 denied request from webapp (team) on dev") || !strings.Contains(body, "1 denied request from webapp (default) on dev") ||
		!strings.Contains(body, "vm=dev&amp;workspace=team\"") {
		t.Errorf("Needs you:\n%s", body)
	}
	// Topology has a node for each sandbox, and the palette an entry.
	body = ts.get("/topology?vm=dev&sel=sb:dev/team/webapp").Body.String()
	if !strings.Contains(body, "sel=sb%3Adev%2Fdefault%2Fwebapp") || !strings.Contains(body, "workspace team") || !strings.Contains(body, `href="/sandboxes/dev/team/webapp?tab=traffic"`) {
		t.Errorf("topology:\n%s", body)
	}
	if body := ts.get("/palette?q=webapp").Body.String(); !strings.Contains(body, "/sandboxes/dev/team/webapp") || !strings.Contains(body, "/sandboxes/dev/default/webapp") || !strings.Contains(body, "on dev · team") {
		t.Errorf("palette:\n%s", body)
	}
	if body := ts.get("/palette?q=pr-review").Body.String(); strings.Contains(body, "on review ·") {
		t.Errorf("palette names the only workspace:\n%s", body)
	}
}

func TestWorkspaceAllowDialog(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	ts.st.AddRequests("dev", []store.Request{
		{Workspace: "team", Sandbox: "webapp", Time: time.Now(), Verdict: "deny", Process: "/usr/bin/node", Host: "team.example", Port: 443, Reason: "no rule"},
	})
	snap := ts.st.Snapshot()
	seq := snap.TrafficOf("dev").Requests[len(snap.TrafficOf("dev").Requests)-1].Seq
	if seq == 0 {
		t.Fatal("no request sequence")
	}
	form := values("vm", "dev", "workspace", "team", "sandbox", "webapp", "mode", "rule", "host", "team.example", "port", "443", "binary", "/usr/bin/node", "protocol", "rest", "access", "read-only", "enforcement", "enforce")
	w := ts.post("/a/allow/preview", form)
	if !strings.Contains(w.Body.String(), "openshell -g brig-dev --workspace team policy update webapp") {
		t.Errorf("preview:\n%s", w.Body)
	}
	// The same form for the other workspace names the other one, and
	// without a workspace there is no command.
	form.Set("workspace", "default")
	if w := ts.post("/a/allow/preview", form); !strings.Contains(w.Body.String(), "--workspace default policy update webapp") {
		t.Errorf("preview for default:\n%s", w.Body)
	}
	form.Del("workspace")
	if w := ts.post("/a/allow/preview", form); strings.Contains(w.Body.String(), "policy update") {
		t.Errorf("a command without a workspace:\n%s", w.Body)
	}
	// Save to file… passes the workspace to brig policy save.
	save := values("vm", "dev", "workspace", "team", "sandbox", "webapp", "to", "webapp")
	if w := ts.post("/a/policy-save-preview/preview", save); !strings.Contains(w.Body.String(), "brig policy save dev webapp --workspace team --to webapp --dry-run") {
		t.Errorf("save preview:\n%s", w.Body)
	}
}
