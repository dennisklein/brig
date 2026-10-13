// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/ocwatch"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// Watch sends the lines after from, then each line logged later, until ctx
// ends.
func (c fakeClient) Watch(ctx context.Context, workspace, sandbox string, from osclient.WatchFrom, each func(osclient.WatchEvent)) error {
	if err := c.err(); err != nil {
		return err
	}
	k := keyOf(workspace, sandbox)
	c.g.mu.Lock()
	if c.g.watches == nil {
		c.g.watches = map[sandboxKey][]osclient.WatchFrom{}
	}
	c.g.watches[k] = append(c.g.watches[k], from)
	lost := c.g.lost
	c.g.mu.Unlock()
	next := 0
	if from.Cursor != "" {
		if lost {
			return osclient.ErrCursorLost
		}
		next, _ = strconv.Atoi(from.Cursor)
	}
	for {
		c.g.mu.Lock()
		lines := slices.Clone(c.g.logs[k][min(next, len(c.g.logs[k])):])
		c.g.mu.Unlock()
		for _, l := range lines {
			next++
			if from.Cursor == "" && l.Time.Before(from.Since) {
				continue
			}
			each(osclient.WatchEvent{Cursor: strconv.Itoa(next), Line: &l})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (c fakeClient) Drafts(_ context.Context, workspace, sandbox string) ([]store.Draft, error) {
	k := keyOf(workspace, sandbox)
	if err := c.err(); err != nil {
		return nil, err
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	if c.g.draftReads == nil {
		c.g.draftReads = map[sandboxKey]int{}
	}
	c.g.draftReads[k]++
	return slices.Clone(c.g.drafts[k]), nil
}

func (g *fakeGateway) log(k sandboxKey, ls ...osclient.LogLine) {
	g.set(func(g *fakeGateway) { g.logs[k] = append(g.logs[k], ls...) })
}

// waitTraffic waits until cond holds for the traffic of dev.
func waitTraffic(t *testing.T, st *store.Store, what string, cond func(store.Traffic) bool) store.Traffic {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tr := st.Snapshot().TrafficOf("dev")
		if cond(tr) {
			return tr
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; traffic: %+v", what, tr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTraffic(t *testing.T) {
	now := time.Now()
	fake := &fakeGateway{
		policyReads: map[sandboxKey]int{}, draftReads: map[sandboxKey]int{},
		sandboxes: []store.Sandbox{{Workspace: "default", Name: "webapp", Phase: "Ready"}},
		logs: map[sandboxKey][]osclient.LogLine{dfltWebapp: {
			// Older than the backfill.
			{Time: now.Add(-2 * time.Hour), Level: "OCSF", Message: "NET:OPEN [INFO] ALLOWED curl(1) -> old.example:443"},
			{Time: now.Add(-time.Minute), Level: "OCSF", Message: "NET:OPEN [INFO] ALLOWED curl(1) -> api.github.com:443"},
			{Time: now.Add(-time.Minute), Level: "INFO", Message: "policy reloaded"},
			{Time: now.Add(-time.Minute), Level: "OCSF", Message: "PROC:LAUNCH [INFO] python3(42)"},
		}},
		drafts: map[sandboxKey][]store.Draft{},
	}
	g := &Gateways{
		Dial:           func(store.VM) (GatewayClient, error) { fake.dials.Add(1); return fakeClient{fake}, nil },
		SandboxesEvery: 5 * time.Millisecond, RestEvery: time.Hour, Backoff: 5 * time.Millisecond,
		FlushEvery: time.Millisecond, DraftsAfter: 20 * time.Millisecond, DraftsEvery: time.Hour,
	}
	st := store.New()
	dev := store.VM{VM: vm.VM{Name: "dev", Ports: vm.Ports{Gateway: 42443}}, State: "running"}
	st.SetVMs(store.VMState{VMs: []store.VM{dev}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx, st); close(done) }()
	defer func() { cancel(); <-done }()

	// The backfill reaches an hour back and keeps only requests.
	waitTraffic(t, st, "the backfill", func(tr store.Traffic) bool { return len(tr.Requests) > 0 })
	time.Sleep(20 * time.Millisecond)
	tr := st.Snapshot().TrafficOf("dev")
	if len(tr.Requests) != 1 || tr.Requests[0].Host != "api.github.com" || tr.Requests[0].Workspace != "default" || tr.Requests[0].Sandbox != "webapp" ||
		!tr.Requests[0].Time.Equal(now.Add(-time.Minute)) {
		t.Fatalf("backfill: %+v", tr.Requests)
	}
	fake.mu.Lock()
	from := fake.watches[dfltWebapp][0]
	reads := fake.draftReads[dfltWebapp]
	fake.mu.Unlock()
	if from.Cursor != "" || from.Tail != ocwatch.TailLines || now.Sub(from.Since) < 59*time.Minute {
		t.Errorf("first watch: %+v", from)
	}
	if reads != 1 {
		t.Errorf("drafts read %d times at the start", reads)
	}

	// A denial has the drafts read again a while later.
	fake.set(func(f *fakeGateway) {
		f.drafts[dfltWebapp] = []store.Draft{{Workspace: "default", Sandbox: "webapp", ID: "ch-1", Rule: "allow_registry_npmjs_org_443"}}
	})
	fake.log(dfltWebapp, osclient.LogLine{Time: time.Now(), Level: "OCSF", Message: "NET:OPEN [MED] DENIED /usr/bin/node(7) -> registry.npmjs.org:443"})
	tr = waitTraffic(t, st, "the draft", func(tr store.Traffic) bool { return len(tr.Drafts) == 1 })
	if len(tr.Requests) != 2 || !tr.Requests[1].Denied() || tr.Requests[1].Process != "/usr/bin/node" {
		t.Errorf("requests: %+v", tr.Requests)
	}

	// A new connection resumes at the cursor: no line twice.
	fake.bundleChanged.Store(true)
	waitFor(t, st, "a second watch", func(store.Gateway) bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.watches[dfltWebapp]) == 2
	})
	fake.mu.Lock()
	from = fake.watches[dfltWebapp][1]
	fake.mu.Unlock()
	if from.Cursor != "5" {
		t.Errorf("second watch: %+v", from)
	}
	fake.log(dfltWebapp, osclient.LogLine{Time: time.Now(), Level: "OCSF", Message: "HTTP:GET [INFO] ALLOWED GET https://api.github.com/user"})
	tr = waitTraffic(t, st, "the next line", func(tr store.Traffic) bool { return len(tr.Requests) == 3 })
	if tr.Requests[2].Path != "/user" {
		t.Errorf("after resuming: %+v", tr.Requests[2])
	}

	// A cursor the gateway lost leaves a gap, then reads from the last
	// line on.
	fake.set(func(f *fakeGateway) { f.lost = true })
	fake.bundleChanged.Store(true)
	tr = waitTraffic(t, st, "the gap", func(tr store.Traffic) bool { return len(tr.Requests) == 4 })
	if tr.Requests[3].Gap == "" {
		t.Errorf("gap: %+v", tr.Requests[3])
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(st.Snapshot().TrafficOf("dev").Requests); n != 4 {
		t.Errorf("%d requests after the gap, want 4", n)
	}

	// A sandbox that is gone loses its drafts; a VM that is gone, its
	// traffic.
	fake.set(func(f *fakeGateway) { f.sandboxes = nil })
	waitTraffic(t, st, "the drafts to go", func(tr store.Traffic) bool { return len(tr.Drafts) == 0 })
	st.SetVMs(store.VMState{})
	deadline := time.Now().Add(5 * time.Second)
	for len(st.Snapshot().Traffic) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(st.Snapshot().Traffic); n != 0 {
		t.Errorf("traffic of %d VMs left", n)
	}
}

func TestTrafficNoDrafts(t *testing.T) {
	c := &noDrafts{fakeClient: fakeClient{&fakeGateway{policyReads: map[sandboxKey]int{}, draftReads: map[sandboxKey]int{}}}}
	st := store.New()
	tr := newTraffic(&Gateways{}, st, "dev", c, &ocwatch.Watcher{})
	sandboxes := []store.Sandbox{{Workspace: "default", Name: "webapp"}}
	tr.readDrafts(context.Background(), sandboxes)
	tr.readDrafts(context.Background(), sandboxes)
	if c.calls != 1 {
		t.Errorf("asked a gateway without drafts %d times", c.calls)
	}
}

type noDrafts struct {
	fakeClient
	calls int
}

func (c *noDrafts) Drafts(context.Context, string, string) ([]store.Draft, error) {
	c.calls++
	return nil, status.Error(codes.Unimplemented, "unknown method GetDraftPolicy")
}

// TestTrafficWorkspaces follows sandboxes of one name in two workspaces: each
// has its own log position, requests and drafts, and a workspace that goes
// ends only its own followers.
func TestTrafficWorkspaces(t *testing.T) {
	now := time.Now()
	line := func(host string) osclient.LogLine {
		return osclient.LogLine{Time: now.Add(-time.Minute), Level: "OCSF", Message: "NET:OPEN [MED] DENIED /usr/bin/node(7) -> " + host + ":443"}
	}
	fake := &fakeGateway{
		policyReads: map[sandboxKey]int{}, draftReads: map[sandboxKey]int{},
		workspaces: []string{"default", "team"},
		sandboxes: []store.Sandbox{
			{Workspace: "default", Name: "webapp", Phase: "Ready"},
			{Workspace: "team", Name: "webapp", Phase: "Ready"},
		},
		logs: map[sandboxKey][]osclient.LogLine{
			dfltWebapp:              {line("default.example")},
			keyOf("team", "webapp"): {line("team.example")},
		},
		drafts: map[sandboxKey][]store.Draft{
			dfltWebapp:              {{ID: "ch-1", Rule: "allow_default"}},
			keyOf("team", "webapp"): {{ID: "ch-1", Rule: "allow_team"}},
		},
	}
	g := &Gateways{
		Dial:           func(store.VM) (GatewayClient, error) { return fakeClient{fake}, nil },
		SandboxesEvery: 5 * time.Millisecond, RestEvery: time.Hour, Backoff: 5 * time.Millisecond,
		FlushEvery: time.Millisecond, DraftsAfter: 20 * time.Millisecond, DraftsEvery: time.Hour,
	}
	st := store.New()
	st.SetVMs(store.VMState{VMs: []store.VM{{VM: vm.VM{Name: "dev", Ports: vm.Ports{Gateway: 42443}}, State: "running"}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx, st); close(done) }()
	defer func() { cancel(); <-done }()

	tr := waitTraffic(t, st, "both workspaces' traffic", func(tr store.Traffic) bool {
		return len(tr.Requests) == 2 && len(tr.Drafts) == 2
	})
	hosts := map[string]string{}
	for _, r := range tr.Requests {
		hosts[r.Workspace+"/"+r.Sandbox] = r.Host
	}
	if hosts["default/webapp"] != "default.example" || hosts["team/webapp"] != "team.example" {
		t.Errorf("requests by sandbox: %v", hosts)
	}
	for _, d := range tr.Drafts {
		if want := "allow_" + d.Workspace; d.Rule != want {
			t.Errorf("draft of %s/%s is %q, want %q", d.Workspace, d.Sandbox, d.Rule, want)
		}
	}
	// The same draft ID in two workspaces is two drafts.
	if d, ok := tr.Draft("team", "webapp", "ch-1"); !ok || d.Rule != "allow_team" {
		t.Errorf("Draft(team) = %+v, %v", d, ok)
	}
	// A request is matched with its own workspace's draft only.
	denied := tr.Requests[0]
	if d, ok := tr.DraftFor(denied); ok && d.Workspace != denied.Workspace {
		t.Errorf("request of %s matched a draft of %s", denied.Workspace, d.Workspace)
	}
	fake.mu.Lock()
	watched := len(fake.watches[dfltWebapp]) == 1 && len(fake.watches[keyOf("team", "webapp")]) == 1
	fake.mu.Unlock()
	if !watched {
		t.Errorf("watches: %v", fake.watches)
	}
	// Each sandbox's policy was read in its workspace.
	gw := waitFor(t, st, "both policies", func(gw store.Gateway) bool {
		return len(gw.Sandboxes) == 2 && gw.Sandboxes[0].Policy != nil && gw.Sandboxes[1].Policy != nil
	})
	for _, s := range gw.Sandboxes {
		if s.Policy.ProviderRules[0] != s.Workspace {
			t.Errorf("policy of %s/%s was read in %s", s.Workspace, s.Name, s.Policy.ProviderRules[0])
		}
	}
	if !slices.Equal(gw.Workspaces, []string{"default", "team"}) {
		t.Errorf("Workspaces = %v", gw.Workspaces)
	}
	if _, ok := gw.Sandbox("team", "webapp"); !ok {
		t.Error("Sandbox(team, webapp) not found")
	}
	if s, ok := gw.Sandbox("default", "webapp"); !ok || s.Workspace != "default" {
		t.Errorf("Sandbox(default, webapp) = %+v, %v", s, ok)
	}

	// The team workspace goes: its sandbox's drafts go, the default's stay.
	fake.set(func(f *fakeGateway) { f.sandboxes = f.sandboxes[:1]; f.workspaces = []string{"default"} })
	tr = waitTraffic(t, st, "the team drafts to go", func(tr store.Traffic) bool { return len(tr.Drafts) == 1 })
	if tr.Drafts[0].Workspace != "default" {
		t.Errorf("drafts left: %+v", tr.Drafts)
	}
}

func TestWorkspacesTooOld(t *testing.T) {
	fake := &fakeGateway{policyReads: map[sandboxKey]int{}}
	g := &Gateways{
		Dial:           func(store.VM) (GatewayClient, error) { return oldGateway{fakeClient{fake}}, nil },
		SandboxesEvery: 5 * time.Millisecond, RestEvery: time.Hour, Backoff: 5 * time.Millisecond,
	}
	st := store.New()
	st.SetVMs(store.VMState{VMs: []store.VM{{VM: vm.VM{Name: "dev", Ports: vm.Ports{Gateway: 42443}}, State: "running"}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx, st); close(done) }()
	defer func() { cancel(); <-done }()
	gw := waitFor(t, st, "the gateway to be refused", func(gw store.Gateway) bool { return gw.State == store.GatewayUnreachable })
	if !strings.Contains(gw.Err, "too old") {
		t.Errorf("Err = %q", gw.Err)
	}
}

// oldGateway has no ListWorkspaces.
type oldGateway struct{ fakeClient }

func (oldGateway) Workspaces(context.Context) ([]string, error) {
	return nil, status.Error(codes.Unimplemented, "unknown method ListWorkspaces")
}
