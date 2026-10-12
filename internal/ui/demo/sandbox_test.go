// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"slices"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

// devGateway returns the demo's gateway of dev after the command argv.
func devGateway(t *testing.T, argv ...string) store.Gateway {
	t.Helper()
	h := New(store.New())
	p, err := h.plan(argv)
	if err != nil {
		t.Fatal(err)
	}
	var g store.Gateway
	for _, gw := range Gateways(time.Now()) {
		if gw.VM == "dev" {
			g = gw
		}
	}
	if p.gw == nil || p.gwVM != "dev" {
		t.Fatalf("plan changes no gateway of dev: %+v", p)
	}
	p.gw(&g)
	return g
}

func names(g store.Gateway) []string {
	var out []string
	for _, s := range g.Sandboxes {
		out = append(out, s.Name)
	}
	return out
}

func TestPlaySandboxCreate(t *testing.T) {
	g := devGateway(t, "brig", "sandbox", "create", "dev", "--mount", "/data/x:/mnt/x", "--", "--name", "aaa", "--template", "claude",
		"--provider", "llm", "--provider", "github", "--policy", "/x/policies/tools.yaml", "--detach", "--no-auto-providers")
	if want := []string{"aaa", "kit-dev", "tools", "webapp"}; !slices.Equal(names(g), want) {
		t.Fatalf("sandboxes %v, want %v", names(g), want)
	}
	s, _ := g.Sandbox("default", "aaa")
	if s.Phase != "Ready" || s.Image != "localhost/claude-agent:2026-10-01" || s.Template != "claude" || !slices.Equal(s.Providers, []string{"github", "llm"}) {
		t.Errorf("sandbox %+v", s)
	}
	if s.Policy == nil || len(s.Policy.ProviderRules) != 2 || s.Policy.Base != toolsPolicy {
		t.Errorf("policy %+v", s.Policy)
	}
}

func TestPlaySandboxChanges(t *testing.T) {
	g := devGateway(t, "openshell", "-g", "brig-dev", "sandbox", "delete", "webapp")
	if want := []string{"kit-dev", "tools"}; !slices.Equal(names(g), want) {
		t.Errorf("after delete: %v, want %v", names(g), want)
	}
	if s, _ := devGateway(t, "openshell", "-g", "brig-dev", "sandbox", "start", "tools").Sandbox("default", "tools"); s.Phase != "Ready" {
		t.Errorf("after start: %s", s.Phase)
	}
	if s, _ := devGateway(t, "openshell", "-g", "brig-dev", "sandbox", "stop", "webapp").Sandbox("default", "webapp"); s.Phase != "Stopped" {
		t.Errorf("after stop: %s", s.Phase)
	}
	s, _ := devGateway(t, "openshell", "-g", "brig-dev", "sandbox", "provider", "attach", "kit-dev", "github", "--wait").Sandbox("default", "kit-dev")
	if !slices.Equal(s.Providers, []string{"github", "llm"}) || s.PolicyVersion != 2 || len(s.Policy.ProviderRules) != 2 {
		t.Errorf("after attach: %+v", s)
	}
	s, _ = devGateway(t, "openshell", "-g", "brig-dev", "sandbox", "provider", "detach", "webapp", "github", "--wait").Sandbox("default", "webapp")
	if !slices.Equal(s.Providers, []string{"llm"}) || len(s.Policy.ProviderRules) != 1 {
		t.Errorf("after detach: %+v", s)
	}
}

func TestPlayRefusesWhatItDoesNotKnow(t *testing.T) {
	h := New(store.New())
	for _, argv := range [][]string{
		{"openshell", "sandbox", "delete", "x"},
		{"openshell", "-g", "other", "sandbox", "delete", "x"},
		{"openshell", "-g", "brig-dev", "provider", "list"},
		{"brig", "sandbox", "create"},
	} {
		if _, err := h.plan(argv); err == nil {
			t.Errorf("played %q", argv)
		}
	}
}

func TestTitle(t *testing.T) {
	for argv, want := range map[string][]string{
		"openshell sandbox delete webapp":                  {"openshell", "-g", "brig-dev", "sandbox", "delete", "webapp"},
		"openshell sandbox provider attach kit-dev github": {"openshell", "-g", "brig-dev", "sandbox", "provider", "attach", "kit-dev", "github", "--wait"},
		"brig sandbox create":                              {"brig", "sandbox", "create", "dev", "--", "--name", "x"},
	} {
		if got := title(want); got != argv {
			t.Errorf("title(%q) = %q, want %q", want, got, argv)
		}
	}
}
