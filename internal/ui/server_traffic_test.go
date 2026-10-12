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

	"github.com/dennisklein/brig/internal/ui/store"
)

// npmDraft is the rule OpenShell drafts from webapp's denied npm requests.
var npmDraft = store.Draft{
	Sandbox: "webapp", ID: "ch-npm", Rule: "allow_registry_npmjs_org_443", Binary: "/usr/bin/node",
	Endpoints: []store.DraftEndpoint{{Host: "registry.npmjs.org", Port: 443}}, Binaries: []string{"/usr/bin/node"},
	YAML: "allow_registry_npmjs_org_443:\n  name: allow_registry_npmjs_org_443\n", Validation: "no new findings", Hits: 3,
}

// addTraffic adds requests to the test server's gateways: on dev, a denial
// with a draft, an allowed request, a line brig ui could not read and a
// gap; on review, a denial of a credential.
func addTraffic(ts *testServer) {
	now := time.Now()
	ts.st.AddRequests("dev", []store.Request{
		{Sandbox: "kit-dev", Time: now.Add(-5 * time.Minute), Gap: "The gateway dropped lines of the log: 3 dropped"},
		{Sandbox: "webapp", Time: now.Add(-4 * time.Minute), Verdict: "allow", Method: "GET", Process: "/usr/bin/gh", Host: "api.github.com", Port: 443, Path: "/user", Policy: "_provider_github", Engine: "l7"},
		{Sandbox: "webapp", Time: now.Add(-3 * time.Minute), Raw: `HTTP:CONNECT [MED] DENIED <script>alert(1)</script>`},
		{Sandbox: "webapp", Time: now.Add(-2 * time.Minute), Verdict: "deny", Process: "/usr/bin/node", PID: 7, Host: "registry.npmjs.org", Port: 443, Reason: "endpoint registry.npmjs.org:443 is not allowed by any policy"},
	})
	ts.st.SetDrafts("dev", "webapp", []store.Draft{npmDraft})
	ts.st.AddRequests("review", []store.Request{
		{Sandbox: "pr-review", Time: now.Add(-time.Minute), Verdict: "deny", Method: "POST", Process: "gh", Host: "uploads.github.com", Port: 443, Path: "/repos/a/b", Reason: "credential_endpoint_mismatch: GH_TOKEN"},
	})
}

func TestTrafficPages(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	addTraffic(ts)
	for target, want := range map[string][]string{
		"/traffic": {
			"registry.npmjs.org:443", "DENY", "ALLOW", "RAW", "Requests of kit-dev may be missing here", "_provider_github",
			`hx-get="/a/allow?req=4&amp;sandbox=webapp&amp;vm=dev"`, `title="OpenShell drafted the rule allow_registry_npmjs_org_443 for this"`,
			"uploads.github.com", `<span class="count alert" title="Denied requests in the last hour">2</span>`,
		},
		"/traffic?vm=review":                          {"uploads.github.com", `class="on" href="/traffic?vm=review"`},
		"/traffic?verdict=deny&vm=dev&sandbox=webapp": {"registry.npmjs.org:443", "Traffic <span class=\"muted\">· webapp</span>"},
		"/sandboxes/dev/webapp?tab=traffic": {
			"Rules OpenShell drafted", "no new findings", "Approve…", `<span class="n alert"`,
			"openshell -g brig-dev logs webapp --tail --source sandbox",
		},
		"/sandboxes/dev/webapp?tab=policy": {"Save to file…", `hx-get="/a/policy-save-preview?sandbox=webapp&amp;vm=dev"`},
		"/":                                {"Recent denials", "1 denied request from webapp on dev in the last hour", "Allow…"},
		"/topology?vm=dev": {
			"fence 1", "registry.npmjs.org", "denied in the last hour", "reached in the last hour", `class="e deny"`,
			`href="/topology?sel=gw%3Adev&amp;vm=dev"`,
		},
		"/topology?vm=dev&sel=ep:registry.npmjs.org:443": {"was denied for", "Allow…", `class="n sel bad"`, "dim"},
		"/topology?sel=pr:dev/github":                    {"sends GH_TOKEN only to api.github.com:443, github.com:443"},
		"/topology?sel=sb:dev/webapp":                    {"See its traffic"},
		"/palette?q=topo":                                {"/topology"},
	} {
		w := ts.get(target)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", target, w.Code)
			continue
		}
		body := w.Body.String()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s lacks %q", target, s)
			}
		}
		if strings.Contains(body, "<script>alert") {
			t.Errorf("%s: a log line was not escaped", target)
		}
	}
	// Denied only leaves out allowed requests.
	if body := ts.get("/traffic?verdict=deny").Body.String(); strings.Contains(body, "_provider_github") {
		t.Error("the denied requests include an allowed one")
	}
	// Filters that are not valid are dropped.
	if body := ts.get("/traffic?vm=Bad..Name&sandbox=x").Body.String(); !strings.Contains(body, "uploads.github.com") {
		t.Error("a bad VM filter hid traffic")
	}
	if body := ts.get("/topology?sel=<x>").Body.String(); strings.Contains(body, `class="pb trace"`) {
		t.Error("a bad selection was traced")
	}
}

func TestTrafficEmpty(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for target, want := range map[string]string{
		"/traffic": "No traffic yet",
		"/":        "No denied requests",
	} {
		if body := ts.get(target).Body.String(); !strings.Contains(body, want) {
			t.Errorf("%s lacks %q", target, want)
		}
	}
}

func TestAllowDialog(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	addTraffic(ts)

	body := ts.get("/a/allow?vm=dev&sandbox=webapp&req=4").Body.String()
	for _, want := range []string{
		"Allow a request for webapp", "registry.npmjs.org:443 by /usr/bin/node was denied: endpoint registry.npmjs.org:443 is not allowed by any policy.",
		`name="mode" value="draft" checked`, `name="draft" value="ch-npm"`, "allow_registry_npmjs_org_443:\n  name:",
		`name="host" value="registry.npmjs.org"`, `name="port" value="443"`, `name="binary" value="/usr/bin/node"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("allow dialog lacks %q", want)
		}
	}

	// Approving the draft runs openshell, and offers to save the rule.
	form := values("vm", "dev", "sandbox", "webapp", "req", "4", "mode", "draft", "draft", "ch-npm")
	form.Set("shown", ts.shownBy("allow", form))
	if w := ts.post("/a/allow/run", form); w.Code != http.StatusOK {
		t.Fatalf("run: %d\n%s", w.Code, w.Body)
	}
	s := ts.starts.started()[0]
	if s.Command.Display != "openshell -g brig-dev rule approve webapp --chunk-id ch-npm" || s.VM != "" || s.Next != "policy-save-preview" ||
		!slices.Equal(s.NextForm, []string{"vm", "dev", "sandbox", "webapp", "rule", "allow_registry_npmjs_org_443"}) {
		t.Errorf("spec: %+v", s)
	}

	// A program the log named only by its name is a guess, with the
	// command that checks it; a credential's denial asks first.
	body = ts.get("/a/allow?vm=review&sandbox=pr-review&req=5").Body.String()
	for _, want := range []string{
		`name="mode" value="rule"`, `name="binary" value="/usr/bin/gh"`, `name="access"`, `value="read-write" selected`,
		"so this path is a guess", `readlink -f &#34;$(command -v gh)&#34;`, "Allow anyway",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("credential dialog lacks %q", want)
		}
	}
	w := ts.post("/a/allow/preview", values("vm", "review", "sandbox", "pr-review", "req", "5", "mode", "rule",
		"host", "uploads.github.com", "port", "443", "binary", "/usr/bin/gh", "protocol", "rest", "access", "read-write", "enforcement", "enforce"))
	if !strings.Contains(w.Body.String(), "tick Allow anyway") || !strings.Contains(w.Body.String(), "disabled") {
		t.Errorf("a credential's denial is allowed without asking:\n%s", w.Body)
	}
}

func TestNextFormFillsTheNextDialog(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	end := time.Now()
	ts.st.AddTask(store.Task{
		ID: "t", Display: "openshell -g brig-dev rule approve webapp --chunk-id ch-npm", Status: store.TaskOK, End: &end,
		Next: "policy-save-preview", NextForm: []string{"vm", "dev", "sandbox", "webapp", "rule", "allow_x"},
	})
	body := ts.get("/").Body.String()
	if !strings.Contains(body, `hx-get="/a/policy-save-preview?rule=allow_x&amp;sandbox=webapp&amp;vm=dev"`) || !strings.Contains(body, "Save to file…") {
		t.Error("the task card does not offer to save the rule")
	}
	body = ts.get("/a/policy-save-preview?rule=allow_x&sandbox=webapp&vm=dev").Body.String()
	if !strings.Contains(body, "Save the rules of webapp to its policy file") || !strings.Contains(body, `name="rule" value="allow_x"`) {
		t.Errorf("save dialog:\n%s", body)
	}
}

func TestTrafficEvents(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	if w := ts.get("/events?f=topology&sel=%3Cx%3E"); w.Code != http.StatusBadRequest {
		t.Errorf("bad selection: %d, want 400", w.Code)
	}

	srv := httptest.NewServer(ts.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events?f=traffic-table&f=vm-table&vm=dev&verdict=deny", nil)
	req.Host = "127.0.0.1:7878"
	req.AddCookie(ts.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	if !sc.Scan() {
		t.Fatalf("stream ended: %v", sc.Err())
	}
	addTraffic(ts)
	var name string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" && name != "" {
			break
		}
		if n, ok := strings.CutPrefix(line, "event: "); ok {
			name = n
		} else if d, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, d)
		}
	}
	// New traffic sends the table, with the page's filters, and not the
	// VM table.
	got := strings.Join(data, "\n")
	if name != "traffic-table" || !strings.Contains(got, "registry.npmjs.org") || strings.Contains(got, "uploads.github.com") || strings.Contains(got, "_provider_github") {
		t.Errorf("got event %q: %.400s", name, got)
	}
}

func TestTrafficRows(t *testing.T) {
	now := time.Now()
	sn := store.Snapshot{Traffic: []store.Traffic{
		{VM: "dev", Requests: []store.Request{
			{Seq: 1, VM: "dev", Sandbox: "webapp", Time: now.Add(-3 * time.Minute), Verdict: "deny", Host: "a.example", Port: 443},
			{Seq: 3, VM: "dev", Sandbox: "webapp", Time: now.Add(-time.Minute), Verdict: "allow", Host: "b.example", Port: 443},
		}},
		{VM: "review", Requests: []store.Request{
			{Seq: 2, VM: "review", Sandbox: "pr-review", Time: now.Add(-2 * time.Minute), Verdict: "deny", Host: "c.example", Port: 443},
		}},
	}}
	seqs := func(rows []trafficRow) []uint64 {
		var out []uint64
		for _, r := range rows {
			out = append(out, r.Seq)
		}
		return out
	}
	for _, c := range []struct {
		q     trafficQuery
		limit int
		want  []uint64
	}{
		{trafficQuery{}, 10, []uint64{3, 2, 1}},
		{trafficQuery{}, 2, []uint64{3, 2}},
		{trafficQuery{Denied: true}, 10, []uint64{2, 1}},
		{trafficQuery{VM: "dev", Sandbox: "webapp"}, 10, []uint64{3, 1}},
	} {
		if got := seqs(trafficRows(sn, c.q, c.limit)); !slices.Equal(got, c.want) {
			t.Errorf("%+v, %d: %v, want %v", c.q, c.limit, got, c.want)
		}
	}
	if n := denialCount(sn, "", "", now.Add(-150*time.Second)); n != 1 {
		t.Errorf("%d recent denials, want 1", n)
	}
	if q := (trafficQuery{VM: "dev", Sandbox: "webapp", Denied: true}); q.URL() != "/traffic?sandbox=webapp&verdict=deny&vm=dev" || q.OnVM("").URL() != "/traffic?verdict=deny" {
		t.Errorf("URLs: %s, %s", q.URL(), q.OnVM("").URL())
	}
}
