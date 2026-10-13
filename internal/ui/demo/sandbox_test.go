// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"slices"
	"strings"
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
	g := devGateway(t, "brig", "sandbox", "create", "dev", "--workspace", "default", "--mount", "/data/x:/mnt/x", "--", "--name", "aaa", "--template", "claude",
		"--provider", "llm", "--provider", "github", "--policy", "/x/policies/tools.yaml", "--detach", "--no-auto-providers")
	if want := []string{"aaa", "kit-dev", "tools", "webapp", "webapp"}; !slices.Equal(names(g), want) {
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
	g := devGateway(t, "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "delete", "webapp")
	if want := []string{"kit-dev", "tools", "webapp"}; !slices.Equal(names(g), want) {
		t.Errorf("after delete: %v, want %v", names(g), want)
	}
	if _, ok := g.Sandbox("team", "webapp"); !ok {
		t.Error("deleting a sandbox deleted the one of the same name in another workspace")
	}
	if s, _ := devGateway(t, "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "start", "tools").Sandbox("default", "tools"); s.Phase != "Ready" {
		t.Errorf("after start: %s", s.Phase)
	}
	if s, _ := devGateway(t, "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "stop", "webapp").Sandbox("default", "webapp"); s.Phase != "Stopped" {
		t.Errorf("after stop: %s", s.Phase)
	}
	s, _ := devGateway(t, "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "provider", "attach", "kit-dev", "github", "--wait").Sandbox("default", "kit-dev")
	if !slices.Equal(s.Providers, []string{"github", "llm"}) || s.PolicyVersion != 2 || len(s.Policy.ProviderRules) != 2 {
		t.Errorf("after attach: %+v", s)
	}
	s, _ = devGateway(t, "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "provider", "detach", "webapp", "github", "--wait").Sandbox("default", "webapp")
	if !slices.Equal(s.Providers, []string{"llm"}) || len(s.Policy.ProviderRules) != 1 {
		t.Errorf("after detach: %+v", s)
	}
}

func TestPlayInTheOtherWorkspace(t *testing.T) {
	g := devGateway(t, "openshell", "-g", "brig-dev", "--workspace", "team", "sandbox", "stop", "webapp")
	if s, _ := g.Sandbox("team", "webapp"); s.Phase != "Stopped" {
		t.Errorf("team's webapp after stop: %s", s.Phase)
	}
	if s, _ := g.Sandbox("default", "webapp"); s.Phase != "Ready" {
		t.Errorf("default's webapp after team's stop: %s", s.Phase)
	}
	g = devGateway(t, "brig", "sandbox", "create", "dev", "--workspace", "team", "--", "--name", "kit-dev", "--template", "pi")
	if s, ok := g.Sandbox("team", "kit-dev"); !ok || s.Image != "localhost/pi-agent:2026-10-08" {
		t.Errorf("team's new sandbox: %+v, %v", s, ok)
	}
	if want := []string{"kit-dev", "tools", "webapp", "kit-dev", "webapp"}; !slices.Equal(names(g), want) {
		t.Errorf("sandboxes %v, want %v", names(g), want)
	}
}

func TestPlayRefusesWhatItDoesNotKnow(t *testing.T) {
	h := New(store.New())
	for _, argv := range [][]string{
		{"openshell", "sandbox", "delete", "x"},
		{"openshell", "-g", "other", "sandbox", "delete", "x"},
		{"openshell", "-g", "brig-dev", "--workspace", "default", "provider", "list"},
		{"openshell", "-g", "brig-dev", "sandbox", "delete", "x"},
		{"brig", "sandbox", "create"},
	} {
		if _, err := h.plan(argv); err == nil {
			t.Errorf("played %q", argv)
		}
	}
}

func TestTitle(t *testing.T) {
	for argv, want := range map[string][]string{
		"openshell sandbox delete webapp":                  {"openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "delete", "webapp"},
		"openshell sandbox provider attach kit-dev github": {"openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "provider", "attach", "kit-dev", "github", "--wait"},
		"brig sandbox create":                              {"brig", "sandbox", "create", "dev", "--", "--name", "x"},
	} {
		if got := title(want); got != argv {
			t.Errorf("title(%q) = %q, want %q", want, got, argv)
		}
	}
}

func TestPlayPolicyAllow(t *testing.T) {
	h := New(store.New())
	allow := []string{"brig", "policy", "allow", "dev", "webapp", "registry.npmjs.org:443", "--workspace", "default", "--binary", "/usr/bin/node", "--rule", "npm", "--access", "read-only"}

	p, err := h.plan(allow)
	if err != nil || p.fail != "" || p.after == nil || !strings.Contains(p.output, "keeps it for new sandboxes") {
		t.Errorf("without --save: %+v, %v", p, err)
	}

	// The demo's sandboxes come from the default policy, which --save
	// refuses before it adds the rule.
	p, err = h.plan(append(slices.Clone(allow), "--save"))
	if err != nil || !strings.Contains(p.fail, "--to webapp") || p.after != nil {
		t.Errorf("--save without --to: %+v, %v", p, err)
	}

	p, err = h.plan(append(slices.Clone(allow), "--save", "--to", "webapp"))
	if err != nil || p.fail != "" || p.after == nil || !strings.Contains(p.output, "loaded policy version 4 with rule npm") || !strings.Contains(p.output, "Created ") {
		t.Errorf("--save --to: %+v, %v", p, err)
	}
	if len(p.steps) < 4 {
		t.Errorf("steps %v lack the save", p.steps)
	}
}

func TestPlayRuleReject(t *testing.T) {
	h := New(store.New())
	h.traffic.drafted("dev", "default", "webapp", patterns["webapp"][3], time.Now())
	reject := []string{"openshell", "-g", "brig-dev", "--workspace", "default", "rule", "reject", "webapp", "--chunk-id", "chunk-registry-npmjs-org", "--reason", "not ours"}

	p, err := h.plan(reject)
	if err != nil || p.fail != "" || p.after == nil || !strings.Contains(p.output, "Rejected draft chunk-registry-npmjs-org") {
		t.Fatalf("plan %+v, %v", p, err)
	}
	p.after()
	if _, ok := h.traffic.draft("dev", "default", "webapp", "chunk-registry-npmjs-org"); ok {
		t.Error("the draft is still pending")
	}
	if p, err = h.plan(reject); err != nil || !strings.Contains(p.fail, "no pending draft") {
		t.Errorf("a second rejection: %+v, %v", p, err)
	}
}
