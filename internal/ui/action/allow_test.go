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
			"brig policy allow dev webapp --workspace default --draft ch-1", "", "vm dev workspace default sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{"draft gone", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-0"), "", "no pending draft", ""},
		{
			"rule", rule(),
			"brig policy allow dev webapp registry.npmjs.org:443 --workspace default --binary /usr/bin/node --rule allow_registry_npmjs_org_443 --access read-only",
			"", "vm dev workspace default sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{
			"rule named, audit", rule("rule", "npm", "enforcement", "audit", "access", "full", "broad", "on"),
			"brig policy allow dev webapp registry.npmjs.org:443 --workspace default --binary /usr/bin/node --rule npm --access full --audit --broad",
			"", "vm dev workspace default sandbox webapp rule npm",
		},
		{
			"whole connection", rule("protocol", "", "broad", "on"),
			"brig policy allow dev webapp registry.npmjs.org:443 --workspace default --binary /usr/bin/node --rule allow_registry_npmjs_org_443 --l4 --broad",
			"", "vm dev workspace default sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{
			"draft of the other workspace", form("vm", "dev", "workspace", "team", "sandbox", "webapp", "mode", "draft", "draft", "ch-2", "req", "3"),
			"brig policy allow dev webapp --workspace team --draft ch-2", "", "vm dev workspace team sandbox webapp rule allow_registry_npmjs_org_443",
		},
		{"draft of the other workspace, asked in this one", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-2"), "", "no pending draft", ""},
		{
			"rule in the other workspace", rule("workspace", "team"),
			"brig policy allow dev webapp registry.npmjs.org:443 --workspace team --binary /usr/bin/node --rule allow_registry_npmjs_org_443 --access read-only",
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
			"brig policy allow dev webapp '*.npmjs.org:443' --workspace default --binary /usr/bin/node --rule allow_wildcard_npmjs_org_443 --access read-only",
			"", "vm dev workspace default sandbox webapp rule allow_wildcard_npmjs_org_443",
		},
		{"host with a path", rule("host", "a.example/x"), "", "Host", ""},
		{"bad port", rule("port", "0"), "", "Port", ""},
		{"no binary", rule("binary", ""), "", "Binary", ""},
		{"relative binary", rule("binary", "node"), "", "full path", ""},
		{"bad rule name", rule("rule", "a b"), "", "Rule name", ""},
		{"provider rule name", rule("rule", "_provider_github"), "", "providers", ""},
		{"full access", rule("access", "full"), "", "Broad rule: full access allows every HTTP method", ""},
		{"whole connection unconfirmed", rule("protocol", ""), "", "Broad rule: the whole connection is allowed", ""},
		{"program wildcard", rule("binary", "/usr/bin/*"), "", "Broad rule: the program path has a wildcard", ""},
		{"bad access", rule("access", "write-ish"), "", "Access", ""},
		{"bad enforcement", rule("enforcement", "block"), "", "Mode", ""},
		{"bad protocol", rule("protocol", "sql"), "", "Inspect", ""},
		{"credential", rule("req", "2", "host", "uploads.github.com"), "", "Allow anyway", ""},
		{
			"credential anyway", rule("req", "2", "host", "uploads.github.com", "anyway", "on"),
			"brig policy allow dev webapp uploads.github.com:443 --workspace default --binary /usr/bin/node --rule allow_uploads_github_com_443 --access read-only",
			"", "vm dev workspace default sandbox webapp rule allow_uploads_github_com_443",
		},
		{
			"draft kept", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-1", "keep", "on", "to", "webapp"),
			"brig policy allow dev webapp --workspace default --draft ch-1 --rule allow_registry_npmjs_org_443 --save --to webapp", "", "",
		},
		{
			"draft kept in the policy file of the sandbox", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-1", "keep", "on"),
			"brig policy allow dev webapp --workspace default --draft ch-1 --rule allow_registry_npmjs_org_443 --save", "", "",
		},
		{
			"rule kept", rule("keep", "on", "to", "webapp"),
			"brig policy allow dev webapp registry.npmjs.org:443 --workspace default --binary /usr/bin/node --rule allow_registry_npmjs_org_443 --access read-only --save --to webapp",
			"", "",
		},
		{
			"audit rule kept in a file by path", rule("keep", "on", "rule", "npm", "enforcement", "audit", "access", "read-write", "to", "~/policies/webapp.yaml"),
			"brig policy allow dev webapp registry.npmjs.org:443 --workspace default --binary /usr/bin/node --rule npm --access read-write --audit --save --to /home/dklein/policies/webapp.yaml",
			"", "",
		},
		{
			"whole connection kept", rule("keep", "on", "protocol", "", "broad", "on"),
			"brig policy allow dev webapp registry.npmjs.org:443 --workspace default --binary /usr/bin/node --rule allow_registry_npmjs_org_443 --l4 --broad --save",
			"", "",
		},
		{
			"full access kept", rule("keep", "on", "access", "full", "broad", "on"),
			"brig policy allow dev webapp registry.npmjs.org:443 --workspace default --binary /usr/bin/node --rule allow_registry_npmjs_org_443 --access full --broad --save",
			"", "",
		},
		{"broad rule kept unconfirmed", rule("keep", "on", "access", "full"), "", "Broad rule", ""},
		{"kept in a bad file", rule("keep", "on", "to", "--force"), "", "File", ""},
		{"keep not ticked, file given", rule("to", "--force"), "brig policy allow dev webapp registry.npmjs.org:443 --workspace default --binary /usr/bin/node --rule allow_registry_npmjs_org_443 --access read-only", "", "vm dev workspace default sandbox webapp rule allow_registry_npmjs_org_443"},
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
	cmd, err = a.Build(form("vm", "dev", "workspace", "default", "sandbox", "webapp", "mode", "draft", "draft", "ch-1", "keep", "on"), trafficContext())
	if err != nil || !cmd.Done || cmd.NextForm != nil {
		t.Errorf("a kept rule offers a next step: %+v, %v", cmd, err)
	}
	save, _ := action.Lookup("policy-save")
	cmd, err = save.Build(form("vm", "dev", "workspace", "default", "sandbox", "webapp", "rule", "npm"), trafficContext())
	if err != nil || cmd.Display != "brig policy save dev webapp --workspace default --rule npm" || cmd.NextForm != nil {
		t.Errorf("save: %q %q, %v", cmd.Display, cmd.NextForm, err)
	}
}

func TestBuildReject(t *testing.T) {
	draft := func(kv ...string) action.Form {
		f := form("vm", "dev", "workspace", "default", "sandbox", "webapp", "draft", "ch-1")
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
	}{
		{"draft", draft(), "openshell -g brig-dev --workspace default rule reject webapp --chunk-id ch-1", ""},
		{"with a reason", draft("reason", "  not our registry "), "openshell -g brig-dev --workspace default rule reject webapp --chunk-id ch-1 --reason 'not our registry'", ""},
		{"long reason", draft("reason", strings.Repeat("x", 201)), "", "Reason"},
		{"reason with a line break", draft("reason", "a\nb"), "", "Reason"},
		{"draft of the other workspace", draft("draft", "ch-2"), "", "no pending draft"},
		{"draft of the workspace", draft("workspace", "team", "draft", "ch-2"), "openshell -g brig-dev --workspace team rule reject webapp --chunk-id ch-2", ""},
		{"no draft", draft("draft", ""), "", "no pending draft"},
		{"stopped VM", form("vm", "airgap", "workspace", "default", "sandbox", "corpus-scan", "draft", "ch-1"), "", "start it before"},
		{"option as draft", draft("draft", "--all"), "", "no pending draft"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _ := action.Lookup("reject")
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
			if cmd.Display != c.want || a.VM(c.f) != "" || a.Next != "" {
				t.Errorf("display %q (next %q),\n want %q", cmd.Display, a.Next, c.want)
			}
		})
	}
}
