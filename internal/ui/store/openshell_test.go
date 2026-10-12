// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

func TestUpdateGateway(t *testing.T) {
	s := New()
	changes, unsubscribe := s.Subscribe()
	defer unsubscribe()
	for _, vm := range []string{"review", "dev", "airgap"} {
		s.UpdateGateway(vm, func(g *Gateway) bool { g.State = GatewayReady; return true })
	}
	before := s.Snapshot()
	var order []string
	for _, g := range before.Gateways {
		order = append(order, g.VM)
	}
	if len(order) != 3 || order[0] != "airgap" || order[1] != "dev" || order[2] != "review" {
		t.Fatalf("gateways out of order: %v", order)
	}
	if n := len(changes); n != 3 {
		t.Errorf("%d notices, want 3", n)
	}

	// No change, no notice.
	s.UpdateGateway("dev", func(*Gateway) bool { return false })
	if n := len(changes); n != 3 {
		t.Errorf("an unchanged gateway was published")
	}

	// A snapshot taken before keeps its gateways.
	s.UpdateGateway("dev", func(g *Gateway) bool { g.Version = "0.1.3"; return true })
	if g, _ := before.Gateway("dev"); g.Version != "" {
		t.Error("an update changed an earlier snapshot")
	}
	if g, _ := s.Snapshot().Gateway("dev"); g.Version != "0.1.3" || g.Name() != "brig-dev" {
		t.Errorf("dev: %+v", g)
	}

	// A new gateway starts stopped.
	s.UpdateGateway("new", func(*Gateway) bool { return false })
	if g, ok := s.Snapshot().Gateway("new"); !ok || g.State != GatewayStopped {
		t.Errorf("new gateway: %+v", g)
	}

	s.RemoveGateways(func(vm string) bool { return vm == "dev" })
	if gws := s.Snapshot().Gateways; len(gws) != 1 || gws[0].VM != "dev" {
		t.Errorf("after removing: %+v", gws)
	}
	if g, _ := before.Gateway("review"); g.VM != "review" {
		t.Error("removing changed an earlier snapshot")
	}
}

func TestSandboxCount(t *testing.T) {
	sn := Snapshot{Gateways: []Gateway{
		{VM: "a", Sandboxes: []Sandbox{{Name: "x", Phase: "Ready"}, {Name: "y", Phase: "Stopped"}}},
		{VM: "b", Sandboxes: []Sandbox{{Name: "z", Phase: "Ready"}}},
	}}
	if ready, total := sn.SandboxCount(); ready != 2 || total != 3 {
		t.Errorf("%d/%d", ready, total)
	}
}

func TestProviderAndProfileLookupPerWorkspace(t *testing.T) {
	g := Gateway{
		Workspaces: []string{"default", "team"},
		Providers: []Provider{
			{Workspace: "default", Name: "llm", Type: "openai"},
			{Workspace: "team", Name: "llm", Type: "team-openai"},
		},
		Profiles: []Profile{
			{ID: "openai", Name: "platform"},
			{Workspace: "default", ID: "github", Name: "default's"},
			{Workspace: "team", ID: "github", Name: "team's"},
			{Workspace: "team", ID: "openai", Name: "team's own"},
		},
	}
	if p, ok := g.Provider("team", "llm"); !ok || p.Type != "team-openai" {
		t.Errorf("Provider(team, llm) = %+v, %v", p, ok)
	}
	if _, ok := g.Provider("other", "llm"); ok {
		t.Error("a provider of another workspace was found")
	}
	for _, c := range []struct{ ws, id, want string }{
		{"default", "github", "default's"},
		{"team", "github", "team's"},
		// A workspace sees the platform's profile unless it has its own.
		{"default", "openai", "platform"},
		{"team", "openai", "team's own"},
	} {
		if p, ok := g.Profile(c.ws, c.id); !ok || p.Name != c.want {
			t.Errorf("Profile(%s, %s) = %+v, %v, want %s", c.ws, c.id, p, ok, c.want)
		}
	}
	if _, ok := g.Profile("default", "nope"); ok {
		t.Error("found a profile that does not exist")
	}
	if !g.MultiWorkspace() || (Gateway{Workspaces: []string{"default"}}).MultiWorkspace() {
		t.Error("MultiWorkspace")
	}
}
