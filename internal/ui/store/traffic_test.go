// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAddRequests(t *testing.T) {
	s := New()
	s.AddRequests("dev", []Request{{Workspace: "default", Sandbox: "webapp", Host: "a.example"}, {Sandbox: "webapp", Host: "b.example"}})
	s.AddRequests("review", []Request{{Sandbox: "pr-review", Host: "c.example"}})
	before := s.Snapshot()
	s.AddRequests("dev", []Request{{Sandbox: "webapp", Host: "d.example"}})
	dev := s.Snapshot().TrafficOf("dev")
	if len(dev.Requests) != 3 || dev.Requests[2].Seq != 4 || dev.Requests[2].VM != "dev" {
		t.Errorf("dev: %+v", dev.Requests)
	}
	if r, ok := dev.Request(4); !ok || r.Host != "d.example" {
		t.Errorf("Request(4) = %+v, %v", r, ok)
	}
	if _, ok := dev.Request(3); ok {
		t.Error("dev has review's request 3")
	}
	// A snapshot keeps what it held.
	if n := len(before.TrafficOf("dev").Requests); n != 2 {
		t.Errorf("an earlier snapshot holds %d requests", n)
	}

	// The oldest go beyond MaxRequests; earlier snapshots keep theirs.
	full := make([]Request, MaxRequests)
	s.AddRequests("dev", full)
	dev = s.Snapshot().TrafficOf("dev")
	if len(dev.Requests) != MaxRequests || dev.Requests[0].Seq != 5 {
		t.Errorf("%d requests from %d", len(dev.Requests), dev.Requests[0].Seq)
	}
	if before.TrafficOf("dev").Requests[1].Host != "b.example" {
		t.Error("trimming changed an earlier snapshot")
	}

	s.RemoveTraffic(func(vm string) bool { return vm == "review" })
	if sn := s.Snapshot(); len(sn.Traffic) != 1 || sn.Traffic[0].VM != "review" {
		t.Errorf("after removing dev: %+v", sn.Traffic)
	}
}

// A sandbox sets the size of the text fields, so the store cuts them.
func TestAddRequestsCutsFields(t *testing.T) {
	s := New()
	long := strings.Repeat("ä", 3*maxRaw)
	s.AddRequests("dev", []Request{{Sandbox: "webapp", Host: "a.example", Path: long, Process: long, Reason: long, Raw: long, Policy: "ok"}})
	r := s.Snapshot().TrafficOf("dev").Requests[0]
	for name, got := range map[string]string{"Path": r.Path, "Process": r.Process, "Reason": r.Reason} {
		if len(got) > maxField+len("…") || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
			t.Errorf("%s: %d bytes, valid %v", name, len(got), utf8.ValidString(got))
		}
	}
	if len(r.Raw) > maxRaw+len("…") || !utf8.ValidString(r.Raw) {
		t.Errorf("Raw: %d bytes", len(r.Raw))
	}
	if r.Host != "a.example" || r.Policy != "ok" || r.Sandbox != "webapp" {
		t.Errorf("short fields changed: %+v", r)
	}
}

func TestSetDrafts(t *testing.T) {
	s := New()
	sub := s.Subscribe()
	defer sub.Close()
	npm := Draft{ID: "ch-1", Rule: "npm", Binary: "/usr/bin/node", Endpoints: []DraftEndpoint{{Host: "registry.npmjs.org", Port: 443}}}
	s.SetDrafts("dev", "default", "webapp", []Draft{npm})
	s.SetDrafts("dev", "default", "kit-dev", []Draft{{ID: "ch-2", Rule: "raw"}})
	if got := sub.Take(); !got[TopicTraffic] {
		t.Errorf("notified %v for new drafts", got)
	}
	// The same drafts again change nothing.
	s.SetDrafts("dev", "default", "webapp", []Draft{npm})
	if got := sub.Take(); len(got) != 0 {
		t.Errorf("notified %v for the same drafts", got)
	}
	tr := s.Snapshot().TrafficOf("dev")
	if len(tr.Drafts) != 2 || tr.Drafts[0].Sandbox != "kit-dev" || tr.Drafts[1].Sandbox != "webapp" {
		t.Errorf("drafts: %+v", tr.Drafts)
	}
	if d, ok := tr.Draft("default", "webapp", "ch-1"); !ok || d.Rule != "npm" {
		t.Errorf("Draft = %+v, %v", d, ok)
	}

	// A draft matches a denial of its endpoint by its program, by path or
	// name, or by any program when the log named none.
	for _, c := range []struct {
		r    Request
		want bool
	}{
		{Request{Workspace: "default", Sandbox: "webapp", Process: "/usr/bin/node", Host: "registry.npmjs.org", Port: 443}, true},
		{Request{Workspace: "default", Sandbox: "webapp", Process: "node", Host: "Registry.npmjs.org", Port: 443}, true},
		{Request{Workspace: "default", Sandbox: "webapp", Host: "registry.npmjs.org", Port: 443}, true},
		{Request{Workspace: "default", Sandbox: "webapp", Process: "/usr/bin/curl", Host: "registry.npmjs.org", Port: 443}, false},
		{Request{Workspace: "default", Sandbox: "webapp", Process: "node", Host: "registry.npmjs.org", Port: 80}, false},
		{Request{Workspace: "default", Sandbox: "kit-dev", Process: "node", Host: "registry.npmjs.org", Port: 443}, false},
	} {
		if _, ok := tr.DraftFor(c.r); ok != c.want {
			t.Errorf("DraftFor(%+v) = %v", c.r, ok)
		}
	}

	s.SetDrafts("dev", "default", "webapp", nil)
	if tr := s.Snapshot().TrafficOf("dev"); len(tr.Drafts) != 1 {
		t.Errorf("drafts after clearing webapp's: %+v", tr.Drafts)
	}
}

func TestRequest(t *testing.T) {
	r := Request{Host: "uploads.github.com", Port: 443, Reason: "credential_endpoint_mismatch: GH_TOKEN"}
	if r.Endpoint() != "uploads.github.com:443" || !r.CredentialMismatch() || r.Denied() {
		t.Errorf("%+v", r)
	}
	if (Request{Host: "a.example"}).Endpoint() != "a.example" {
		t.Error("an endpoint without a port")
	}
}

// TestDraftsPerWorkspace keeps the drafts of a sandbox name in two
// workspaces apart.
func TestDraftsPerWorkspace(t *testing.T) {
	s := New()
	s.SetDrafts("dev", "default", "webapp", []Draft{{ID: "ch-1", Rule: "a", Endpoints: []DraftEndpoint{{Host: "x.example", Port: 443}}}})
	s.SetDrafts("dev", "team", "webapp", []Draft{{ID: "ch-1", Rule: "b", Endpoints: []DraftEndpoint{{Host: "x.example", Port: 443}}}})
	tr := s.Snapshot().TrafficOf("dev")
	if len(tr.Drafts) != 2 || tr.Drafts[0].Workspace != "default" || tr.Drafts[1].Workspace != "team" {
		t.Fatalf("drafts: %+v", tr.Drafts)
	}
	for ws, rule := range map[string]string{"default": "a", "team": "b"} {
		if d, ok := tr.Draft(ws, "webapp", "ch-1"); !ok || d.Rule != rule {
			t.Errorf("Draft(%s) = %+v, %v", ws, d, ok)
		}
		d, ok := tr.DraftFor(Request{Workspace: ws, Sandbox: "webapp", Host: "x.example", Port: 443})
		if !ok || d.Rule != rule {
			t.Errorf("DraftFor in %s = %+v, %v", ws, d, ok)
		}
	}
	if _, ok := tr.Draft("other", "webapp", "ch-1"); ok {
		t.Error("found a draft in a workspace that has none")
	}
	// Clearing one workspace's drafts leaves the other's.
	s.SetDrafts("dev", "team", "webapp", nil)
	if tr := s.Snapshot().TrafficOf("dev"); len(tr.Drafts) != 1 || tr.Drafts[0].Workspace != "default" {
		t.Errorf("after clearing team's: %+v", tr.Drafts)
	}
}

func TestGatewaySandboxPerWorkspace(t *testing.T) {
	g := Gateway{Sandboxes: []Sandbox{
		{Workspace: "default", Name: "webapp", Phase: "Ready"},
		{Workspace: "team", Name: "webapp", Phase: "Stopped"},
	}}
	if s, ok := g.Sandbox("team", "webapp"); !ok || s.Phase != "Stopped" {
		t.Errorf("Sandbox(team) = %+v, %v", s, ok)
	}
	if s, ok := g.Sandbox("default", "webapp"); !ok || s.Phase != "Ready" {
		t.Errorf("Sandbox(default) = %+v, %v", s, ok)
	}
	if _, ok := g.Sandbox("other", "webapp"); ok {
		t.Error("found a sandbox in a workspace that has none")
	}
}
