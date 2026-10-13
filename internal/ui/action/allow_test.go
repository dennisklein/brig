// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/demo"
	"github.com/dennisklein/brig/internal/ui/store"
)

// trafficContext is the demo host with denials on dev's webapp, one with a
// draft and one of a credential.
func trafficContext() action.Context {
	c := demoContext()
	c.Snap.Gateways = demo.Gateways(time.Now())
	c.Snap.Traffic = []store.Traffic{{
		VM: "dev",
		Requests: []store.Request{
			{Seq: 1, VM: "dev", Workspace: "default", Sandbox: "webapp", Verdict: "deny", Process: "/usr/bin/node", Host: "registry.npmjs.org", Port: 443},
			{Seq: 2, VM: "dev", Workspace: "default", Sandbox: "webapp", Verdict: "deny", Method: "POST", Host: "uploads.github.com", Port: 443, Reason: "credential_endpoint_mismatch"},
			{Seq: 3, VM: "dev", Workspace: "team", Sandbox: "webapp", Verdict: "deny", Process: "/usr/bin/node", Host: "registry.npmjs.org", Port: 443},
		},
		Drafts: []store.Draft{{
			Workspace: "default", Sandbox: "webapp", ID: "ch-1", Rule: "allow_registry_npmjs_org_443", Binary: "/usr/bin/node",
			Endpoints: []store.DraftEndpoint{{Host: "registry.npmjs.org", Port: 443}}, Security: "new host",
		}, {
			Workspace: "team", Sandbox: "webapp", ID: "ch-2", Rule: "allow_registry_npmjs_org_443", Binary: "/usr/bin/node",
			Endpoints: []store.DraftEndpoint{{Host: "registry.npmjs.org", Port: 443}},
		}},
	}}
	return c
}

func TestBuildAllow(t *testing.T) {
	// rule is a rule's form with the fields kv changed.
	rule := func(kv ...string) action.Form {
		f := form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "rule", "host", "registry.npmjs.org", "port", "443",
			"binary", "/usr/bin/node", "protocol", "rest", "access", "read-only", "enforcement", "enforce")
		for i := 0; i+1 < len(kv); i += 2 {
			f.Set(kv[i], kv[i+1])
		}
		return f
	}
	for _, c := range []struct {
		name string
		f    action.Form
		want string // the command, or "" if Build must refuse
		err  string // part of the refusal
		next string // the fields of the next dialog
	}{
		{
			"draft", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-1", "req", "1"),
			"openshell -g brig-dev --workspace default rule approve webapp --chunk-id ch-1", "", "vm dev workspace default sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{"draft gone", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-0"), "", "no pending draft", ""},
		{
			"rule", rule(),
			"openshell -g brig-dev --workspace default policy update webapp --rule-name allow_registry_npmjs_org_443 --binary /usr/bin/node --add-endpoint registry.npmjs.org:443:read-only:rest:enforce --wait",
			"", "vm dev workspace default sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{
			"rule named, audit", rule("rule", "npm", "enforcement", "audit", "access", "full"),
			"openshell -g brig-dev --workspace default policy update webapp --rule-name npm --binary /usr/bin/node --add-endpoint registry.npmjs.org:443:full:rest:audit --wait",
			"", "vm dev workspace default sandbox webapp rule npm",
		},
		{
			"whole connection", rule("protocol", ""),
			"openshell -g brig-dev --workspace default policy update webapp --rule-name allow_registry_npmjs_org_443 --binary /usr/bin/node --add-endpoint registry.npmjs.org:443 --wait",
			"", "vm dev workspace default sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{
			"draft of the other workspace", form("vm", "dev", "workspace", "team", "sandbox", "webapp", "mode", "draft", "draft", "ch-2", "req", "3"),
			"openshell -g brig-dev --workspace team rule approve webapp --chunk-id ch-2", "", "vm dev workspace team sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{"draft of the other workspace, asked in this one", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-2"), "", "no pending draft", ""},
		{
			"rule in the other workspace", rule("workspace", "team"),
			"openshell -g brig-dev --workspace team policy update webapp --rule-name allow_registry_npmjs_org_443 --binary /usr/bin/node --add-endpoint registry.npmjs.org:443:read-only:rest:enforce --wait",
			"", "vm dev workspace team sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{"no workspace", form("vm", "dev", "sandbox", "webapp", "mode", "draft"), "", "no workspace name", ""},
		{"no mode", form("vm", "dev", "workspace", "default", "sandbox", "webapp"), "", "choose between", ""},
		{"stopped VM", form("vm", "airgap", "workspace", "default", "sandbox", "corpus-scan", "mode", "draft"), "", "start it before", ""},
		{"unknown sandbox", form("vm", "dev", "workspace", "default", "sandbox", "nope", "mode", "draft"), "", "brig-dev has no sandbox nope", ""},
		{"option as sandbox", form("vm", "dev", "workspace", "default", "sandbox", "--help", "mode", "draft"), "", "no sandbox", ""},
		{"bad host", rule("host", "a:b"), "", "Host", ""},
		{"any host", rule("host", "**"), "", "Host: a wildcard needs a domain", ""},
		{"every host", rule("host", "*"), "", "Host: a wildcard needs a domain", ""},
		{"a whole top-level domain", rule("host", "*.com", "port", "1", "binary", "/bin/sh", "protocol", ""), "", "Host: a wildcard needs a domain", ""},
		{
			"a wildcard below a domain", rule("host", "*.npmjs.org"),
			"openshell -g brig-dev --workspace default policy update webapp --rule-name allow_wildcard_npmjs_org_443 --binary /usr/bin/node --add-endpoint '*.npmjs.org:443:read-only:rest:enforce' --wait",
			"", "vm dev workspace default sandbox webapp rule allow_wildcard_npmjs_org_443",
		},
		{"host with a path", rule("host", "a.example/x"), "", "Host", ""},
		{"bad port", rule("port", "0"), "", "Port", ""},
		{"no binary", rule("binary", ""), "", "Binary", ""},
		{"relative binary", rule("binary", "node"), "", "full path", ""},
		{"bad rule name", rule("rule", "a b"), "", "Rule name", ""},
		{"provider rule name", rule("rule", "_provider_github"), "", "providers", ""},
		{"bad access", rule("access", "write-ish"), "", "Access", ""},
		{"bad enforcement", rule("enforcement", "block"), "", "Mode", ""},
		{"bad protocol", rule("protocol", "sql"), "", "Inspect", ""},
		{"credential", rule("req", "2", "host", "uploads.github.com"), "", "Allow anyway", ""},
		{
			"credential anyway", rule("req", "2", "host", "uploads.github.com", "anyway", "on"),
			"openshell -g brig-dev --workspace default policy update webapp --rule-name allow_uploads_github_com_443 --binary /usr/bin/node --add-endpoint uploads.github.com:443:read-only:rest:enforce --wait",
			"", "vm dev workspace default sandbox webapp rule allow_uploads_github_com_443",
		},
		{
			"save preview", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "rule", "npm"),
			"brig policy save dev webapp --workspace default --rule npm --dry-run", "", "vm dev workspace default sandbox webapp rule npm",
		},
		{
			"save preview to", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "to", "webapp"),
			"brig policy save dev webapp --workspace default --to webapp --dry-run", "", "vm dev workspace default sandbox webapp to webapp",
		},
		{
			"save preview to a path", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "to", "~/policies/webapp.yaml"),
			"brig policy save dev webapp --workspace default --to /home/dklein/policies/webapp.yaml --dry-run", "", "vm dev workspace default sandbox webapp to /home/dklein/policies/webapp.yaml",
		},
		{
			"save preview in the other workspace", form("vm", "dev", "workspace", "team", "sandbox", "webapp", "to", "webapp"),
			"brig policy save dev webapp --workspace team --to webapp --dry-run", "", "vm dev workspace team sandbox webapp to webapp",
		},
		{"save preview bad file", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "to", "--force"), "", "File", ""},
		{"save preview bad rule", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "rule", "-x"), "", "Rule", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := "allow"
			if strings.HasPrefix(c.name, "save") {
				id = "policy-save-preview"
			}
			a, _ := action.Lookup(id)
			cmd, err := a.Build(c.f, trafficContext())
			if c.want == "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("got %q and error %v, want an error with %q", cmd.Display, err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Display != c.want {
				t.Errorf("display %q,\n want %q", cmd.Display, c.want)
			}
			if got := strings.Join(cmd.NextForm, " "); got != c.next {
				t.Errorf("next form %q, want %q", got, c.next)
			}
			if a.VM(c.f) != "" {
				t.Error("an OpenShell change locks the VM")
			}
		})
	}
}

func TestBuildAllowNotes(t *testing.T) {
	a, _ := action.Lookup("allow")
	cmd, err := a.Build(form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-1"), trafficContext())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(cmd.Notes, func(n string) bool { return strings.Contains(n, "security notes: new host") }) {
		t.Errorf("notes: %q", cmd.Notes)
	}
	save, _ := action.Lookup("policy-save")
	cmd, err = save.Build(form("vm", "dev", "workspace", "default", "sandbox", "webapp", "rule", "npm"), trafficContext())
	if err != nil || cmd.Display != "brig policy save dev webapp --workspace default --rule npm" || cmd.NextForm != nil {
		t.Errorf("save: %q %q, %v", cmd.Display, cmd.NextForm, err)
	}
}
