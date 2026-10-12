// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
)

func TestAddRequests(t *testing.T) {
	s := New()
	s.AddRequests("dev", []Request{{Sandbox: "webapp", Host: "a.example"}, {Sandbox: "webapp", Host: "b.example"}})
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

func TestSetDrafts(t *testing.T) {
	s := New()
	ch, unsubscribe := s.Subscribe()
	defer unsubscribe()
	npm := Draft{ID: "ch-1", Rule: "npm", Binary: "/usr/bin/node", Endpoints: []DraftEndpoint{{Host: "registry.npmjs.org", Port: 443}}}
	s.SetDrafts("dev", "webapp", []Draft{npm})
	s.SetDrafts("dev", "kit-dev", []Draft{{ID: "ch-2", Rule: "raw"}})
	<-ch
	<-ch
	// The same drafts again change nothing.
	s.SetDrafts("dev", "webapp", []Draft{npm})
	select {
	case got := <-ch:
		t.Errorf("notified %q for the same drafts", got)
	default:
	}
	tr := s.Snapshot().TrafficOf("dev")
	if len(tr.Drafts) != 2 || tr.Drafts[0].Sandbox != "kit-dev" || tr.Drafts[1].Sandbox != "webapp" {
		t.Errorf("drafts: %+v", tr.Drafts)
	}
	if d, ok := tr.Draft("webapp", "ch-1"); !ok || d.Rule != "npm" {
		t.Errorf("Draft = %+v, %v", d, ok)
	}

	// A draft matches a denial of its endpoint by its program, by path or
	// name, or by any program when the log named none.
	for _, c := range []struct {
		r    Request
		want bool
	}{
		{Request{Sandbox: "webapp", Process: "/usr/bin/node", Host: "registry.npmjs.org", Port: 443}, true},
		{Request{Sandbox: "webapp", Process: "node", Host: "Registry.npmjs.org", Port: 443}, true},
		{Request{Sandbox: "webapp", Host: "registry.npmjs.org", Port: 443}, true},
		{Request{Sandbox: "webapp", Process: "/usr/bin/curl", Host: "registry.npmjs.org", Port: 443}, false},
		{Request{Sandbox: "webapp", Process: "node", Host: "registry.npmjs.org", Port: 80}, false},
		{Request{Sandbox: "kit-dev", Process: "node", Host: "registry.npmjs.org", Port: 443}, false},
	} {
		if _, ok := tr.DraftFor(c.r); ok != c.want {
			t.Errorf("DraftFor(%+v) = %v", c.r, ok)
		}
	}

	s.SetDrafts("dev", "webapp", nil)
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
