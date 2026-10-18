// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

// npmDraft is the rule OpenShell drafts from webapp's denied npm requests.
var npmDraft = store.Draft{
	Workspace: "default", Sandbox: "webapp", ID: "ch-npm", Rule: "allow_registry_npmjs_org_443", Binary: "/usr/bin/node",
	Endpoints: []store.DraftEndpoint{{Host: "registry.npmjs.org", Port: 443}}, Binaries: []string{"/usr/bin/node"},
	YAML: "allow_registry_npmjs_org_443:\n  name: allow_registry_npmjs_org_443\n", Validation: "no new findings", Hits: 3,
}

// addTraffic adds requests to the test server's gateways: on dev, a denial
// with a draft, an allowed request, a line brig ui could not read and a
// gap; on review, a denial of a credential.
func addTraffic(ts *testServer) {
	now := time.Now()
	ts.st.AddRequests("dev", []store.Request{
		{Workspace: "default", Sandbox: "kit-dev", Time: now.Add(-5 * time.Minute), Gap: "The gateway dropped lines of the log: 3 dropped"},
		{Workspace: "default", Sandbox: "webapp", Time: now.Add(-4 * time.Minute), Verdict: "allow", Method: "GET", Process: "/usr/bin/gh", Host: "api.github.com", Port: 443, Path: "/user", Policy: "_provider_github", Engine: "l7"},
		{Workspace: "default", Sandbox: "webapp", Time: now.Add(-3 * time.Minute), Raw: `HTTP:CONNECT [MED] DENIED <script>alert(1)</script>`},
		{Workspace: "default", Sandbox: "webapp", Time: now.Add(-2 * time.Minute), Verdict: "deny", Process: "/usr/bin/node", PID: 7, Host: "registry.npmjs.org", Port: 443, Reason: "endpoint registry.npmjs.org:443 is not allowed by any policy"},
	})
	ts.st.SetDrafts("dev", "default", "webapp", []store.Draft{npmDraft})
	ts.st.AddRequests("review", []store.Request{
		{Workspace: "default", Sandbox: "pr-review", Time: now.Add(-time.Minute), Verdict: "deny", Method: "POST", Process: "gh", Host: "uploads.github.com", Port: 443, Path: "/repos/a/b", Reason: "credential_endpoint_mismatch: GH_TOKEN"},
	})
}

func TestTrafficPages(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	addTraffic(ts)
	for target, want := range map[string][]string{
		"/traffic": {
			"registry.npmjs.org:443", "DENY", "ALLOW", "RAW", "Requests of kit-dev (default) may be missing here", "_provider_github",
			`hx-get="/a/allow?req=4&amp;sandbox=webapp&amp;vm=dev&amp;workspace=default"`, `title="OpenShell drafted the rule allow_registry_npmjs_org_443 for this"`,
			"uploads.github.com", `<span class="count alert" title="Denied requests in the last hour">2</span>`,
		},
		"/traffic?vm=review": {"uploads.github.com", `class="on" href="/traffic?vm=review"`},
		"/traffic?verdict=deny&vm=dev&workspace=default&sandbox=webapp": {"registry.npmjs.org:443", "Traffic <span class=\"muted\">· webapp (default)</span>"},
		"/sandboxes/dev/default/webapp?tab=traffic": {
			"Rules OpenShell drafted", "no new findings", "Approve…", `hx-get="/a/reject?draft=ch-npm&amp;sandbox=webapp&amp;vm=dev&amp;workspace=default"`, `<span class="n alert"`,
			"openshell -g brig-dev --workspace default logs webapp --tail --source sandbox",
		},
		"/sandboxes/dev/default/webapp?tab=policy": {"Save to file…", `hx-get="/a/policy-save-preview?sandbox=webapp&amp;vm=dev&amp;workspace=default"`},
		"/": {"Recent denials", "1 denied request from webapp (default) on dev in the last hour", "Allow…"},
		"/topology?vm=dev": {
			"fence 1", "registry.npmjs.org", "denied in the last hour", "reached in the last hour", `class="e deny"`,
			`href="/topology?sel=gw%3Adev&amp;vm=dev"`,
		},
		"/topology?vm=dev&sel=ep:registry.npmjs.org:443": {"was denied for", "Allow…", `class="n sel bad"`, "dim"},
		"/topology?sel=pr:dev/default/github":            {"sends GH_TOKEN only to api.github.com:443, github.com:443"},
		"/topology?sel=sb:dev/default/webapp":            {"See its traffic"},
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

	body := ts.get("/a/allow?vm=dev&workspace=default&sandbox=webapp&req=4").Body.String()
	for _, want := range []string{
		"Allow a request for webapp (default)", "registry.npmjs.org:443 by /usr/bin/node was denied: endpoint registry.npmjs.org:443 is not allowed by any policy.",
		`name="mode" value="draft" checked`, `name="draft" value="ch-npm"`, "allow_registry_npmjs_org_443:\n  name:",
		`name="host" value="registry.npmjs.org"`, `name="port" value="443"`, `name="binary" value="/usr/bin/node"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("allow dialog lacks %q", want)
		}
	}

	// Approving the draft runs brig policy allow, and offers to save the
	// rule.
	form := values("vm", "dev", "workspace", "default", "sandbox", "webapp", "req", "4", "mode", "draft", "draft", "ch-npm")
	form.Set("shown", ts.shownBy("allow", form))
	if w := ts.post("/a/allow/run", form); w.Code != http.StatusOK {
		t.Fatalf("run: %d\n%s", w.Code, w.Body)
	}
	s := ts.starts.started()[0]
	if s.Command.Display != "brig policy allow dev webapp --workspace default --draft ch-npm" || s.VM != "" || s.Next != "policy-save-preview" ||
		!slices.Equal(s.NextForm, []string{"vm", "dev", "workspace", "default", "sandbox", "webapp", "rule", "allow_registry_npmjs_org_443"}) {
		t.Errorf("spec: %+v", s)
	}

	// Kept, it saves the rule in the same step, and offers nothing after.
	form = values("vm", "dev", "workspace", "default", "sandbox", "webapp", "req", "4", "mode", "draft", "draft", "ch-npm", "keep", "on", "to", "webapp")
	form.Set("shown", ts.shownBy("allow", form))
	if w := ts.post("/a/allow/run", form); w.Code != http.StatusOK {
		t.Fatalf("run: %d\n%s", w.Code, w.Body)
	}
	s = ts.starts.started()[1]
	if s.Command.Display != "brig policy allow dev webapp --workspace default --draft ch-npm --rule allow_registry_npmjs_org_443 --save --to webapp" || s.Next != "" || s.NextForm != nil || !s.ShowOutput {
		t.Errorf("kept spec: %+v", s)
	}
	for _, want := range []string{`name="keep"`, `name="to"`, `name="broad"`} {
		if !strings.Contains(body, want) {
			t.Errorf("allow dialog lacks %q", want)
		}
	}

	// A program the log named only by its name is a guess, with the
	// command that checks it; a credential's denial asks first.
	body = ts.get("/a/allow?vm=review&workspace=default&sandbox=pr-review&req=5").Body.String()
	for _, want := range []string{
		`name="mode" value="rule"`, `name="binary" value="/usr/bin/gh"`, `name="access"`, `value="read-write" selected`,
		"so this path is a guess", `readlink -f &#34;$(command -v gh)&#34;`, "Allow anyway",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("credential dialog lacks %q", want)
		}
	}
	w := ts.post("/a/allow/preview", values("vm", "review", "workspace", "default", "sandbox", "pr-review", "req", "5", "mode", "rule",
		"host", "uploads.github.com", "port", "443", "binary", "/usr/bin/gh", "protocol", "rest", "access", "read-write", "enforcement", "enforce"))
	if !strings.Contains(w.Body.String(), "tick Allow anyway") || !strings.Contains(w.Body.String(), "disabled") {
		t.Errorf("a credential's denial is allowed without asking:\n%s", w.Body)
	}
}

func TestRejectDialog(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	addTraffic(ts)

	body := ts.get("/a/reject?vm=dev&workspace=default&sandbox=webapp&draft=ch-npm").Body.String()
	for _, want := range []string{"Reject the draft rule for webapp (default)", `name="draft" value="ch-npm"`, "allow_registry_npmjs_org_443:\n  name:", `name="reason"`} {
		if !strings.Contains(body, want) {
			t.Errorf("reject dialog lacks %q", want)
		}
	}
	if body = ts.get("/a/reject?vm=dev&workspace=default&sandbox=webapp&draft=gone").Body.String(); !strings.Contains(body, "no such draft any more") {
		t.Errorf("a draft that is gone:\n%s", body)
	}

	form := values("vm", "dev", "workspace", "default", "sandbox", "webapp", "draft", "ch-npm", "reason", "not ours")
	form.Set("shown", ts.shownBy("reject", form))
	if w := ts.post("/a/reject/run", form); w.Code != http.StatusOK {
		t.Fatalf("run: %d\n%s", w.Code, w.Body)
	}
	s := ts.starts.started()[0]
	if s.Command.Display != "openshell -g brig-dev --workspace default rule reject webapp --chunk-id ch-npm --reason 'not ours'" || s.VM != "" || s.Next != "" {
		t.Errorf("spec: %+v", s)
	}
}

func TestNextFormFillsTheNextDialog(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	end := time.Now()
	ts.st.AddTask(store.Task{
		ID: "t", Display: "openshell -g brig-dev --workspace default rule approve webapp --chunk-id ch-npm", Status: store.TaskOK, End: &end,
		Next: "policy-save-preview", NextForm: []string{"vm", "dev", "workspace", "default", "sandbox", "webapp", "rule", "allow_x"},
	})
	body := ts.get("/").Body.String()
	if !strings.Contains(body, `hx-get="/a/policy-save-preview?rule=allow_x&amp;sandbox=webapp&amp;vm=dev&amp;workspace=default"`) || !strings.Contains(body, "Save to file…") {
		t.Error("the task card does not offer to save the rule")
	}
	body = ts.get("/a/policy-save-preview?rule=allow_x&sandbox=webapp&vm=dev&workspace=default").Body.String()
	if !strings.Contains(body, "Save the rules of webapp (default) to its policy file") || !strings.Contains(body, `name="rule" value="allow_x"`) {
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
			{Seq: 1, VM: "dev", Workspace: "default", Sandbox: "webapp", Time: now.Add(-3 * time.Minute), Verdict: "deny", Host: "a.example", Port: 443},
			{Seq: 3, VM: "dev", Workspace: "default", Sandbox: "webapp", Time: now.Add(-time.Minute), Verdict: "allow", Host: "b.example", Port: 443},
			{Seq: 4, VM: "dev", Workspace: "team", Sandbox: "webapp", Time: now.Add(-90 * time.Second), Verdict: "deny", Host: "d.example", Port: 443},
		}},
		{VM: "review", Requests: []store.Request{
			{Seq: 2, VM: "review", Workspace: "default", Sandbox: "pr-review", Time: now.Add(-2 * time.Minute), Verdict: "deny", Host: "c.example", Port: 443},
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
		{trafficQuery{}, 10, []uint64{3, 4, 2, 1}},
		{trafficQuery{}, 2, []uint64{3, 4}},
		{trafficQuery{Denied: true}, 10, []uint64{4, 2, 1}},
		// Two sandboxes of one name are told apart by their workspace.
		{trafficQuery{VM: "dev", Workspace: "default", Sandbox: "webapp"}, 10, []uint64{3, 1}},
		{trafficQuery{VM: "dev", Workspace: "team", Sandbox: "webapp"}, 10, []uint64{4}},
		{trafficQuery{Workspace: "default"}, 10, []uint64{3, 2, 1}},
	} {
		if got := seqs(trafficRows(sn, c.q, c.limit)); !slices.Equal(got, c.want) {
			t.Errorf("%+v, %d: %v, want %v", c.q, c.limit, got, c.want)
		}
	}
	if n := denialCount(sn, "", "", "", now.Add(-150*time.Second)); n != 2 {
		t.Errorf("%d recent denials, want 2", n)
	}
	if n := denialCount(sn, "dev", "team", "webapp", now.Add(-time.Hour)); n != 1 {
		t.Errorf("%d denials of team's webapp, want 1", n)
	}
	q := trafficQuery{VM: "dev", Workspace: "team", Sandbox: "webapp", Denied: true}
	if q.URL() != "/traffic?sandbox=webapp&verdict=deny&vm=dev&workspace=team" || q.OnVM("").URL() != "/traffic?verdict=deny&workspace=team" || q.OnWorkspace("").URL() != "/traffic?verdict=deny&vm=dev" {
		t.Errorf("URLs: %s, %s, %s", q.URL(), q.OnVM("").URL(), q.OnWorkspace("").URL())
	}
	// A sandbox filter needs its VM and workspace.
	if got := trafficQueryOf(url.Values{"sandbox": {"webapp"}, "vm": {"dev"}}); got.Sandbox != "" {
		t.Errorf("a sandbox without a workspace filters: %+v", got)
	}
}

// Many requests of a sandbox to one endpoint make one edge.
func TestTopologyEdges(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	var rs []store.Request
	for range 500 {
		rs = append(rs,
			store.Request{Workspace: "default", Sandbox: "webapp", Time: now.Add(-time.Minute), Verdict: "allow", Host: "x.example", Port: 443},
			store.Request{Workspace: "default", Sandbox: "webapp", Time: now.Add(-time.Minute), Verdict: "deny", Host: "x.example", Port: 443})
	}
	ts.st.AddRequests("dev", rs)
	v := ts.srv.newView(httptest.NewRequest("GET", "/topology", nil), "topology", "")
	topo := topologyOf(v, "dev", "")
	var n int
	for _, e := range topo.Edges {
		if e.To == "ep:x.example:443" {
			n++
			if e.Class != "deny" {
				t.Errorf("edge class %q, want deny", e.Class)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d edges to the endpoint, want 1", n)
	}
}

// After a watch lost its place, older lines follow newer ones. The counts
// of denials look at every line.
func TestDenialsOutOfOrder(t *testing.T) {
	now := time.Now()
	sn := store.Snapshot{Traffic: []store.Traffic{{VM: "dev", Requests: []store.Request{
		{Seq: 1, VM: "dev", Workspace: "default", Sandbox: "webapp", Time: now.Add(-5 * time.Minute), Verdict: "deny", Host: "a.example", Port: 443},
		{Seq: 2, VM: "dev", Workspace: "default", Sandbox: "webapp", Time: now.Add(-3 * time.Hour), Verdict: "deny", Host: "old.example", Port: 443},
		{Seq: 3, VM: "dev", Workspace: "default", Sandbox: "webapp", Time: now.Add(-time.Minute), Verdict: "deny", Host: "b.example", Port: 443},
		{Seq: 4, VM: "dev", Workspace: "default", Sandbox: "webapp", Time: now.Add(-2 * time.Minute), Verdict: "deny", Host: "c.example", Port: 443},
	}}}}
	if n := denialCount(sn, "dev", "default", "webapp", now.Add(-time.Hour)); n != 3 {
		t.Errorf("%d denials in the last hour, want 3", n)
	}
	if got := oldestShown(sn, trafficQuery{}); !got.Equal(now.Add(-3 * time.Hour)) {
		t.Errorf("oldest shown %v", got)
	}
	needs := trafficNeeds(sn)
	if len(needs) != 1 || !strings.HasPrefix(needs[0].Title, "3 denied requests") || !strings.Contains(needs[0].Detail, "b.example") {
		t.Errorf("needs: %+v", needs)
	}
}
