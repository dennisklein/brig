// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/ui/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// Watch sends the lines after from, then each line logged later, until ctx
// ends.
func (c fakeClient) Watch(ctx context.Context, sandbox string, from osclient.WatchFrom, each func(osclient.WatchEvent)) error {
	if err := c.err(); err != nil {
		return err
	}
	c.g.mu.Lock()
	if c.g.watches == nil {
		c.g.watches = map[string][]osclient.WatchFrom{}
	}
	c.g.watches[sandbox] = append(c.g.watches[sandbox], from)
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
		lines := slices.Clone(c.g.logs[sandbox][min(next, len(c.g.logs[sandbox])):])
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

func (c fakeClient) Drafts(_ context.Context, sandbox string) ([]store.Draft, error) {
	if err := c.err(); err != nil {
		return nil, err
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	if c.g.draftReads == nil {
		c.g.draftReads = map[string]int{}
	}
	c.g.draftReads[sandbox]++
	return slices.Clone(c.g.drafts[sandbox]), nil
}

func (g *fakeGateway) log(sandbox string, ls ...osclient.LogLine) {
	g.set(func(g *fakeGateway) { g.logs[sandbox] = append(g.logs[sandbox], ls...) })
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
		policyReads: map[string]int{}, draftReads: map[string]int{},
		sandboxes: []store.Sandbox{{Name: "webapp", Phase: "Ready"}},
		logs: map[string][]osclient.LogLine{"webapp": {
			// Older than the backfill.
			{Time: now.Add(-2 * time.Hour), Level: "OCSF", Message: "NET:OPEN [INFO] ALLOWED curl(1) -> old.example:443"},
			{Time: now.Add(-time.Minute), Level: "OCSF", Message: "NET:OPEN [INFO] ALLOWED curl(1) -> api.github.com:443"},
			{Time: now.Add(-time.Minute), Level: "INFO", Message: "policy reloaded"},
			{Time: now.Add(-time.Minute), Level: "OCSF", Message: "PROC:LAUNCH [INFO] python3(42)"},
		}},
		drafts: map[string][]store.Draft{},
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
	if len(tr.Requests) != 1 || tr.Requests[0].Host != "api.github.com" || tr.Requests[0].Sandbox != "webapp" ||
		!tr.Requests[0].Time.Equal(now.Add(-time.Minute)) {
		t.Fatalf("backfill: %+v", tr.Requests)
	}
	fake.mu.Lock()
	from := fake.watches["webapp"][0]
	reads := fake.draftReads["webapp"]
	fake.mu.Unlock()
	if from.Cursor != "" || from.Tail != tailLines || now.Sub(from.Since) < 59*time.Minute {
		t.Errorf("first watch: %+v", from)
	}
	if reads != 1 {
		t.Errorf("drafts read %d times at the start", reads)
	}

	// A denial has the drafts read again a while later.
	fake.set(func(f *fakeGateway) {
		f.drafts["webapp"] = []store.Draft{{Sandbox: "webapp", ID: "ch-1", Rule: "allow_registry_npmjs_org_443"}}
	})
	fake.log("webapp", osclient.LogLine{Time: time.Now(), Level: "OCSF", Message: "NET:OPEN [MED] DENIED /usr/bin/node(7) -> registry.npmjs.org:443"})
	tr = waitTraffic(t, st, "the draft", func(tr store.Traffic) bool { return len(tr.Drafts) == 1 })
	if len(tr.Requests) != 2 || !tr.Requests[1].Denied() || tr.Requests[1].Process != "/usr/bin/node" {
		t.Errorf("requests: %+v", tr.Requests)
	}

	// A new connection resumes at the cursor: no line twice.
	fake.bundleChanged.Store(true)
	waitFor(t, st, "a second watch", func(store.Gateway) bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.watches["webapp"]) == 2
	})
	fake.mu.Lock()
	from = fake.watches["webapp"][1]
	fake.mu.Unlock()
	if from.Cursor != "5" {
		t.Errorf("second watch: %+v", from)
	}
	fake.log("webapp", osclient.LogLine{Time: time.Now(), Level: "OCSF", Message: "HTTP:GET [INFO] ALLOWED GET https://api.github.com/user"})
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
	c := &noDrafts{fakeClient: fakeClient{&fakeGateway{policyReads: map[string]int{}, draftReads: map[string]int{}}}}
	st := store.New()
	tr := newTraffic(&Gateways{}, st, "dev", c, map[string]*position{})
	sandboxes := []store.Sandbox{{Name: "webapp"}}
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

func (c *noDrafts) Drafts(context.Context, string) ([]store.Draft, error) {
	c.calls++
	return nil, status.Error(codes.Unimplemented, "unknown method GetDraftPolicy")
}

func TestTrafficLogsWhyItCannotWatch(t *testing.T) {
	var buf bytes.Buffer
	c := &watchFails{fakeClient: fakeClient{&fakeGateway{}}}
	g := &Gateways{Backoff: time.Millisecond, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	tr := newTraffic(g, store.New(), "dev", c, map[string]*position{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.watch(ctx, "webapp", &position{}); close(done) }()
	for c.tries.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	// Once, however often it tried.
	if got := strings.Count(buf.String(), "cannot follow"); got != 1 || !strings.Contains(buf.String(), "sandbox=webapp") ||
		!strings.Contains(buf.String(), "permission denied") {
		t.Errorf("log:\n%s", buf.String())
	}
}

type watchFails struct {
	fakeClient
	tries atomic.Int32
}

func (c *watchFails) Watch(context.Context, string, osclient.WatchFrom, func(osclient.WatchEvent)) error {
	c.tries.Add(1)
	return status.Error(codes.PermissionDenied, "permission denied")
}
