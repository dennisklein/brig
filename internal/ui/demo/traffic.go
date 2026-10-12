// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"fmt"
	"math/rand/v2"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/brig/internal/ui/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
)

// trafficEvery is how often a demo sandbox makes a request.
const trafficEvery = 2 * time.Second

// pattern is a request that a demo sandbox makes now and then.
type pattern struct {
	program string
	pid     int
	method  string
	url     string
	// rule is the rule that allows it; empty for a denied request,
	// with reason.
	rule, reason string
	weight       int
}

// patterns are the requests of the demo's sandboxes, as in the design mock.
var patterns = map[string][]pattern{
	"webapp": {
		{"/usr/bin/node", 4211, "POST", "https://models.example.org/v1/chat/completions", "_provider_llm", "", 6},
		{"/usr/bin/git", 4388, "GET", "https://github.com/acme/webapp.git/info/refs?service=git-upload-pack", "_provider_github", "", 3},
		{"/usr/bin/gh", 4402, "GET", "https://api.github.com/repos/acme/webapp/pulls/412", "_provider_github", "", 2},
		{"/usr/bin/node", 4211, "", "registry.npmjs.org:443", "", "endpoint registry.npmjs.org:443 is not allowed by any policy", 2},
		{"/usr/bin/curl", 5120, "", "api.github.com:443", "", "binary /usr/bin/curl is not allowed for endpoint api.github.com:443", 1},
	},
	"kit-dev": {
		{"/usr/bin/node", 3107, "POST", "https://models.example.org/v1/chat/completions", "_provider_llm", "", 5},
		{"/usr/bin/node", 3107, "", "raw.githubusercontent.com:443", "", "endpoint raw.githubusercontent.com:443 is not allowed by any policy", 1},
		{"/usr/bin/node", 3107, "GET", "http://10.0.2.2:11434/api/tags", "ollama_host", "", 1},
	},
	"pr-review": {
		{"/usr/local/bin/claude", 2901, "POST", "https://api.anthropic.com/v1/messages", "_provider_claude-code", "", 6},
		{"/usr/bin/gh", 3015, "GET", "https://api.github.com/repos/acme/webapp/pulls/412/files", "_provider_github", "", 3},
		{"/usr/bin/gh", 3015, "POST", "https://uploads.github.com/repos/acme/webapp/releases/9/assets", "", "credential_endpoint_mismatch: GH_TOKEN is not bound to uploads.github.com", 1},
	},
}

// endpoint returns the host:port of the pattern.
func (p pattern) endpoint() string {
	if p.method == "" {
		return p.url
	}
	u, _ := url.Parse(p.url)
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	return u.Hostname() + ":" + port
}

// line writes the request as a sandbox's supervisor logs it.
func (p pattern) line(allowedBy string) string {
	verdict, sev, ctx := "ALLOWED", "INFO", " [policy:"+allowedBy+" engine:l7]"
	if allowedBy == "" {
		verdict, sev, ctx = "DENIED", "MED", " [policy:- engine:opa] [reason:"+p.reason+"]"
		if strings.HasPrefix(p.reason, "credential") {
			ctx = " [policy:_provider_github engine:l7] [reason:" + p.reason + "]"
		}
	}
	actor := fmt.Sprintf("%s(%d)", p.program, p.pid)
	if p.method == "" {
		return fmt.Sprintf("NET:OPEN [%s] %s %s -> %s%s", sev, verdict, actor, p.url, ctx)
	}
	return fmt.Sprintf("HTTP:%s [%s] %s %s -> %s %s%s", p.method, sev, verdict, actor, p.method, p.url, ctx)
}

// draftOf is the rule OpenShell drafts from a pattern's denials, if it
// drafts one.
func draftOf(ws, sandbox string, p pattern) (store.Draft, bool) {
	if p.rule != "" || strings.HasPrefix(p.reason, "credential") || strings.HasPrefix(p.reason, "binary") {
		return store.Draft{}, false
	}
	host, port, _ := strings.Cut(p.endpoint(), ":")
	portN, _ := strconv.Atoi(port)
	name := "allow_" + strings.NewReplacer(".", "_", "-", "_").Replace(host) + "_" + port
	return store.Draft{
		Workspace: ws, Sandbox: sandbox, ID: "chunk-" + strings.ReplaceAll(host, ".", "-"), Rule: name, Binary: p.program,
		Endpoints: []store.DraftEndpoint{{Host: host, Port: portN}},
		Binaries:  []string{p.program},
		YAML: name + ":\n  name: " + name + "\n  endpoints:\n    - host: " + host + "\n      port: " + port +
			"\n      protocol: rest\n      enforcement: enforce\n      access: read-only\n  binaries:\n    - path: " + p.program + "\n",
		Rationale:  p.program + " was denied reaching " + host + ":" + port + "; a read-only rule for this program covers what it tried.",
		Validation: "No new exfiltration paths: read-only access to one host, for one program.",
	}, true
}

// traffic is the demo's traffic: what sandboxes request, and what rules
// added since allow.
type traffic struct {
	st *store.Store
	mu sync.Mutex
	// allowed maps vm/workspace/sandbox/host:port to the rule that allows
	// it now, with the program it allows.
	allowed map[string]allowedBy
	// drafts are the pending drafts by vm/workspace/sandbox.
	drafts map[string][]store.Draft
}

type allowedBy struct{ rule, program string }

func newTraffic(st *store.Store) *traffic {
	return &traffic{st: st, allowed: map[string]allowedBy{}, drafts: map[string][]store.Draft{}}
}

// backfill makes the last hour's requests of the running sandboxes, with a
// line brig ui cannot read and a gap.
func (t *traffic) backfill(now time.Time, vms []store.VM) {
	for i := 70; i > 0; i-- {
		at := now.Add(-time.Duration(i)*50*time.Second - time.Duration(rand.IntN(40))*time.Second) //nolint:gosec // G404: made-up times
		t.tick(at, vms)
	}
	t.st.AddRequests("dev", []store.Request{{Workspace: store.DefaultWorkspace, Sandbox: "kit-dev", Time: now.Add(-41 * time.Minute), Gap: "The gateway dropped lines of the log: 12 lines dropped, buffer full"}})
	raw, _ := osclient.ParseRequest("HTTP:CONNECT [MED] DENIED tunnel to 192.0.2.7:8443 refused by the upstream proxy")
	raw.Workspace, raw.Sandbox, raw.Time = store.DefaultWorkspace, "webapp", now.Add(-23*time.Minute)
	t.st.AddRequests("dev", []store.Request{raw})
}

// tick makes a request of a running sandbox at the time at.
func (t *traffic) tick(at time.Time, vms []store.VM) {
	type live struct{ vm, workspace, sandbox string }
	var sandboxes []live
	for _, v := range vms {
		if !v.Running() {
			continue
		}
		gw, _ := t.st.Snapshot().Gateway(v.Name)
		for _, s := range gw.Sandboxes {
			if s.Ready() && patterns[s.Name] != nil {
				sandboxes = append(sandboxes, live{v.Name, s.Workspace, s.Name})
			}
		}
	}
	if len(sandboxes) == 0 {
		return
	}
	sb := sandboxes[rand.IntN(len(sandboxes))] //nolint:gosec // G404: made-up traffic
	p := pick(patterns[sb.sandbox])
	t.mu.Lock()
	rule := p.rule
	if a, ok := t.allowed[sb.vm+"/"+sb.workspace+"/"+sb.sandbox+"/"+p.endpoint()]; rule == "" && ok && a.program == p.program {
		rule = a.rule
	}
	t.mu.Unlock()
	r, ok := osclient.ParseRequest(p.line(rule))
	if !ok {
		return
	}
	r.Workspace, r.Sandbox, r.Time = sb.workspace, sb.sandbox, at
	t.st.AddRequests(sb.vm, []store.Request{r})
	if r.Denied() {
		t.drafted(sb.vm, sb.workspace, sb.sandbox, p, at)
	}
}

// drafted counts a denial in the draft OpenShell made from it.
func (t *traffic) drafted(vm, ws, sandbox string, p pattern, at time.Time) {
	d, ok := draftOf(ws, sandbox, p)
	if !ok {
		return
	}
	key := vm + "/" + ws + "/" + sandbox
	t.mu.Lock()
	drafts := slices.Clone(t.drafts[key])
	i := slices.IndexFunc(drafts, func(x store.Draft) bool { return x.ID == d.ID })
	if i < 0 {
		drafts = append(drafts, d)
		i = len(drafts) - 1
	}
	drafts[i].Hits++
	drafts[i].LastSeen = at
	t.drafts[key] = drafts
	t.mu.Unlock()
	t.st.SetDrafts(vm, ws, sandbox, drafts)
}

// allow lets the sandbox's program reach the endpoint from now on, by the
// rule, and drops the drafts the rule covers.
func (t *traffic) allow(vm, ws, sandbox, endpoint, program, rule string) {
	key := vm + "/" + ws + "/" + sandbox
	t.mu.Lock()
	t.allowed[key+"/"+endpoint] = allowedBy{rule: rule, program: program}
	host, _, _ := strings.Cut(endpoint, ":")
	drafts := slices.DeleteFunc(slices.Clone(t.drafts[key]), func(d store.Draft) bool {
		return slices.ContainsFunc(d.Endpoints, func(e store.DraftEndpoint) bool { return e.Host == host })
	})
	t.drafts[key] = drafts
	t.mu.Unlock()
	t.st.SetDrafts(vm, ws, sandbox, drafts)
}

// draft returns the pending draft of the sandbox with the ID id.
func (t *traffic) draft(vm, ws, sandbox, id string) (store.Draft, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := vm + "/" + ws + "/" + sandbox
	i := slices.IndexFunc(t.drafts[key], func(d store.Draft) bool { return d.ID == id })
	if i < 0 {
		return store.Draft{}, false
	}
	return t.drafts[key][i], true
}

func pick(ps []pattern) pattern {
	total := 0
	for _, p := range ps {
		total += p.weight
	}
	n := rand.IntN(total) //nolint:gosec // G404: made-up traffic
	for _, p := range ps {
		if n -= p.weight; n < 0 {
			return p
		}
	}
	return ps[0]
}
