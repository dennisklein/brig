// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/demo"
	"github.com/dennisklein/brig/internal/ui/store"
)

func TestOpenShellPages(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for target, want := range map[string][]string{
		"/sandboxes":                                  {"corpus-scan", "pr-review", "4/6"},
		"/sandboxes?vm=review":                        {"pr-review"},
		"/sandboxes/dev/default/webapp":               {"OPENAI_API_KEY", "models.example.org:443", "openshell -g brig-dev --workspace default sandbox connect webapp"},
		"/sandboxes/dev/default/webapp?tab=policy":    {`<span class="add">`, "# from provider github", "policy get webapp --base", "brig policy diff dev webapp --workspace default"},
		"/sandboxes/dev/default/webapp?tab=files":     {"sandbox download webapp /sandbox/PATH ."},
		"/sandboxes/dev/default/tools?tab=bogus":      {"Connect to the sandbox, once it runs"},
		"/sandboxes/airgap/default/corpus-scan":       {"airgap is not running", "brig start airgap"},
		"/gateways/review":                            {"not on the gateway", "brig sync review --dry-run", "openshell -g brig-review --workspace default provider list"},
		"/gateways/dev?tab=profiles":                  {"GITLAB_TOKEN", "as header PRIVATE-TOKEN", "~/src/agent-openshell/profiles/github.yaml"},
		"/gateways/dev?tab=policies":                  {"OPENSHELL_SANDBOX_POLICY", "~/src/agent-openshell/policies/tools.yaml"},
		"/gateways/dev?tab=templates":                 {"localhost/claude-agent:2026-10-01", "tools"},
		"/gateways/airgap?tab=providers":              {"by hand"},
		"/vms/dev?tab=sandboxes":                      {"kit-dev", "New sandbox…"},
		"/":                                           {"sandboxes", "webapp"},
		"/palette?q=webapp":                           {"/sandboxes/dev/default/webapp"},
		"/palette?q=brig-review%20%C2%B7%20templates": {"/gateways/review?tab=templates"},
	} {
		w := ts.get(target)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", target, w.Code)
			continue
		}
		for _, s := range want {
			if body := w.Body.String(); !strings.Contains(body, s) {
				t.Errorf("%s lacks %q", target, s)
			}
		}
	}
	if w := ts.get("/gateway"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/gateways/dev" {
		t.Errorf("/gateway: %d to %q", w.Code, w.Header().Get("Location"))
	}
	for _, target := range []string{
		"/sandboxes/dev/default/nope", "/sandboxes/nope/default/webapp", "/sandboxes/dev/default/..webapp", "/gateways/nope", "/gateways/Bad..Name",
	} {
		if w := ts.get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, w.Code)
		}
	}
}

func TestGatewayWithoutVMs(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	ts.st.RemoveGateways(func(string) bool { return false })
	if w := ts.get("/gateway"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "no OpenShell gateways") {
		t.Errorf("/gateway: %d", w.Code)
	}
	if w := ts.get("/sandboxes"); !strings.Contains(w.Body.String(), "No VMs with an OpenShell gateway yet") {
		t.Error("an empty list does not say why")
	}
}

func TestOpenShellEscapesUntrustedText(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	ts.st.UpdateGateway("dev", func(gw *store.Gateway) bool {
		gw.Sandboxes = slices.Clone(gw.Sandboxes)
		gw.Sandboxes[2].Problem = `<img src=x onerror=alert(1)>`
		p := *gw.Sandboxes[2].Policy
		p.Effective += "  evil: \"<script>alert(1)</script>\"\n"
		gw.Sandboxes[2].Policy = &p
		return true
	})
	for _, target := range []string{"/sandboxes/dev/default/webapp", "/sandboxes/dev/default/webapp?tab=policy"} {
		body := ts.get(target).Body.String()
		if strings.Contains(body, "<img src=x") || strings.Contains(body, "<script>alert") {
			t.Errorf("%s: text from the gateway was not escaped", target)
		}
	}
}

func TestOpenShellEvents(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for _, target := range []string{
		"/events?f=sandbox&gw=Bad..Name", "/events?f=sandbox&gw=dev&ws=default&sb=..x", "/events?f=sandbox&sb=webapp&ws=default",
		"/events?f=sandbox&gw=dev&sb=webapp", "/events?f=sandbox&gw=dev&ws=..&sb=webapp", "/events?f=sandbox-table&wsfilter=..",
		"/events?f=sandbox-table&filter=Bad..Name",
	} {
		if w := ts.get(target); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", target, w.Code)
		}
	}

	srv := httptest.NewServer(ts.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events?f=sandbox&f=vm-table&gw=dev&ws=default&sb=webapp&tab=policy", nil)
	req.Host = "127.0.0.1:7878"
	req.AddCookie(ts.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	openingEvents(t, sc, "sandbox", "vm-table")
	ts.st.UpdateGateway("dev", func(gw *store.Gateway) bool {
		gw.State = store.GatewayUnreachable
		gw.Err = "brig-dev does not answer"
		return true
	})
	name, data := nextEvent(t, sc)
	// A gateway change sends the sandbox, on its tab, and not the VM table.
	if name != "sandbox" || !strings.Contains(data, "brig-dev does not answer") || !strings.Contains(data, "policy get webapp --full") {
		t.Errorf("got event %q: %.300s", name, data)
	}
}

func TestPolicyLines(t *testing.T) {
	text := "version: 1\nnetwork_policies:\n  _provider_gitlab_work:\n    name: _provider_gitlab_work\n    endpoints:\n      - host: gitlab.example.org\n  pypi:\n    name: pypi\n"
	lines := policyLines(text, []string{"_provider_gitlab_work"}, []string{"gitlab-work"})
	var got []string
	for _, l := range lines {
		got = append(got, l.Provider+"|"+l.Key)
	}
	want := []string{"|version", "|network_policies", "gitlab-work|_provider_gitlab_work", "gitlab-work|name", "gitlab-work|endpoints", "gitlab-work|host", "|pypi", "|name"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if !lines[2].First || lines[3].First {
		t.Error("only a rule's first line is marked first")
	}
	if l := lines[5]; l.Indent != "      - " || l.Rest != " gitlab.example.org" {
		t.Errorf("list item: %+v", l)
	}
}

func TestProviderGroups(t *testing.T) {
	var review store.Gateway
	for _, g := range demo.Gateways(time.Now()) {
		if g.VM == "review" {
			review = g
		}
	}
	var got []string
	groups := providerGroups(review)
	if len(groups) != 1 || groups[0].Workspace != "default" || groups[0].Heading {
		t.Fatalf("a gateway with one workspace has one group without a heading: %+v", groups)
	}
	for _, r := range groups[0].Rows {
		got = append(got, r.Name+" "+strings.Join(r.Sandboxes, ",")+" "+map[bool]string{true: "missing", false: "ok"}[r.Missing])
	}
	want := []string{"claude-code pr-review ok", "github pr-review ok", "gitlab-work  missing", "llm  ok"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestGatewayNeeds(t *testing.T) {
	now := time.Now()
	sn := store.Snapshot{VMs: demo.VMs(now), Gateways: demo.Gateways(now)}
	sn.Gateways[1].State, sn.Gateways[1].Err = store.GatewayUnreachable, "brig-dev does not answer"
	sn.Gateways[2].Version = "0.2.0"
	sn.Gateways[2].Sandboxes = []store.Sandbox{{Name: "pr-review", Phase: "Error", Problem: "image pull failed"}}
	var got []string
	for _, n := range gatewayNeeds(sn) {
		got = append(got, n.Sev+" "+n.Title+" | "+n.Detail)
	}
	want := []string{
		"bad brig ui cannot reach brig-dev | brig-dev does not answer",
		"warn brig-review runs OpenShell 0.2.0 | brig ui reads gateways as OpenShell 0.1.4 does; some details may be missing.",
		"bad Sandbox pr-review on review failed | image pull failed",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	// A gateway of a VM that just booted may still be starting.
	sn.VMs[0].Up = now
	if n := gatewayNeeds(sn); len(n) != 2 {
		t.Errorf("a starting gateway needs the user: %+v", n)
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"openshell", "-g", "brig-dev", "sandbox", "exec", "-n", "web", "--", "sh", "-c", "it's $HOME", ""})
	want := `openshell -g brig-dev sandbox exec -n web -- sh -c 'it'\''s $HOME' ''`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := providerRuleName("GitLab-Work"); got != "_provider_gitlab_work" {
		t.Errorf("providerRuleName: %s", got)
	}
}
