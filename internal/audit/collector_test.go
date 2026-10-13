// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/policy"
	"github.com/dennisklein/brig/internal/ui/store"
)

var created = time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)

const (
	pypiRule = `
  pypi:
    name: pypi
    endpoints:
      - host: pypi.org
        port: 443
    binaries:
      - path: /usr/bin/pip
`
	githubRule = `
  github:
    name: github
    endpoints:
      - host: api.github.com
        port: 443
    binaries:
      - path: /usr/bin/gh
`
	npmRule = `
  npm:
    name: npm
    endpoints:
      - host: registry.npmjs.org
        port: 443
    binaries:
      - path: /usr/bin/npm
`
)

func rev(v uint32, at time.Time, rules string, prov map[string]string) osclient.Revision {
	return osclient.Revision{
		Version: v, Hash: "h" + strconv.FormatUint(uint64(v), 10), Status: "loaded", Created: at,
		Policy: "version: 1\nnetwork_policies:" + rules, Provenance: prov,
	}
}

// fakeGateway has a sandbox webapp with four policy revisions and two
// drafts, and sends two requests on the first watch of its log.
type fakeGateway struct {
	mu        sync.Mutex
	sandboxes []store.Sandbox
	revs      []osclient.Revision
	drafts    []store.Draft
	watches   []osclient.WatchFrom
	down      bool
}

func newFakeGateway() *fakeGateway {
	t3 := created.Add(3 * time.Hour)
	return &fakeGateway{
		sandboxes: []store.Sandbox{{Workspace: "default", Name: "webapp", Image: "agent:1", Providers: []string{"github"}, Created: created, PolicyVersion: 4}},
		revs: []osclient.Revision{
			rev(1, created, pypiRule, nil),
			rev(2, created.Add(time.Hour), pypiRule+githubRule, map[string]string{policy.AnnotationBy: policy.ByPolicyAllow, policy.AnnotationTrace: "trace1"}),
			rev(3, t3, pypiRule+githubRule+npmRule, nil),
			rev(4, created.Add(4*time.Hour), githubRule+npmRule, nil),
		},
		drafts: []store.Draft{
			{ID: "c1", Rule: "npm", Status: "approved", Decided: t3.Add(time.Second), Endpoints: []store.DraftEndpoint{{Host: "registry.npmjs.org", Port: 443}}, Binary: "/usr/bin/npm", Hits: 3},
			{ID: "c2", Rule: "cdn", Status: "pending", Endpoints: []store.DraftEndpoint{{Host: "cdn.example.com", Port: 443}}, Binary: "/usr/bin/curl", Hits: 1},
		},
	}
}

func (g *fakeGateway) Version(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return "", errors.New("down")
	}
	return "0.1.3", nil
}

func (g *fakeGateway) Sandboxes(context.Context) ([]store.Sandbox, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.sandboxes), nil
}

func (g *fakeGateway) Revisions(_ context.Context, _, _ string, after uint32) ([]osclient.Revision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []osclient.Revision
	for _, r := range g.revs {
		if r.Version > after {
			out = append(out, r)
		}
	}
	return out, nil
}

func (g *fakeGateway) AllDrafts(context.Context, string, string) ([]store.Draft, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.drafts), nil
}

func (g *fakeGateway) Watch(ctx context.Context, _, _ string, from osclient.WatchFrom, each func(osclient.WatchEvent)) error {
	g.mu.Lock()
	g.watches = append(g.watches, from)
	first := len(g.watches) == 1
	g.mu.Unlock()
	if first {
		now := time.Now()
		each(osclient.WatchEvent{Cursor: "1", Line: &osclient.LogLine{Time: now, Level: "OCSF", Message: "NET:OPEN [INFO] ALLOWED /usr/bin/pip(42) -> pypi.org:443 [policy:pypi engine:mechanistic]"}})
		each(osclient.WatchEvent{Cursor: "2", Line: &osclient.LogLine{Time: now, Level: "OCSF", Message: "NET:OPEN [MED] DENIED /usr/bin/curl(43) -> cdn.example.com:443 [reason:no_rule]"}})
	}
	<-ctx.Done()
	return ctx.Err()
}

func (g *fakeGateway) Close() error { return nil }

// logCommand appends a finished brig command that names vm to the activity
// log at path.
func logCommand(t *testing.T, path, vm, command string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	log := progress.NewLog(f, progress.LogOptions{Program: "brig", Version: "v0.6.0"})
	bus := progress.NewBus(progress.BusOptions{Sinks: []progress.Sink{log}})
	_, span := progress.Start(progress.WithBus(context.Background(), bus), progress.KindCommand, command)
	span.End(nil, progress.Node(vm))
	bus.Close()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// collect runs a collector of VM dev over g until done reports true for the
// records written.
func collect(t *testing.T, s Store, g *fakeGateway, activityLog string, done func([]Record) bool) []Record {
	t.Helper()
	c := &Collector{
		VM: "dev", Store: s, ActivityLog: activityLog,
		Dial:           func() (Gateway, error) { return g, nil },
		SandboxesEvery: 10 * time.Millisecond, PoliciesEvery: 10 * time.Millisecond, FlushEvery: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- c.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	var recs []Record
	for {
		recs = recs[:0]
		if err := s.Read(Query{}, func(r Record) bool { recs = append(recs, r); return true }); err != nil {
			t.Fatal(err)
		}
		if done(recs) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-errc
			t.Fatalf("records so far: %+v", recs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	return recs
}

func kinds(recs []Record, kind string) []Record {
	var out []Record
	for _, r := range recs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func TestCollector(t *testing.T) {
	s := Store{Root: t.TempDir()}
	g := newFakeGateway()
	activityLog := filepath.Join(t.TempDir(), "activity.jsonl")
	logCommand(t, activityLog, "dev", "policy allow dev webapp api.github.com")
	logCommand(t, activityLog, "other", "start other")
	recs := collect(t, s, g, activityLog, func(recs []Record) bool {
		return len(kinds(recs, KindPolicy)) == 4 && len(kinds(recs, KindRequest)) == 2 && len(kinds(recs, KindDraft)) == 2 && len(kinds(recs, KindAction)) == 1
	})

	sb := kinds(recs, KindSandbox)
	if len(sb) != 1 || sb[0].Event != EventCreated || sb[0].Image != "agent:1" || !sb[0].T.Equal(created) || sb[0].VM != "dev" || sb[0].V != Version {
		t.Errorf("sandbox records: %+v", sb)
	}
	pol := kinds(recs, KindPolicy)
	type want struct {
		by                      string
		added, removed, changed []string
	}
	for i, w := range []want{
		{by: BySandboxCreate, added: []string{"pypi"}},
		{by: ByPolicyAllow, added: []string{"github"}},
		{by: "draft c1 approved", added: []string{"npm"}},
		{by: ByUnknown, removed: []string{"pypi"}},
	} {
		p := pol[i]
		if p.Version != uint32(i+1) || p.By != w.by || !slices.Equal(p.Added, w.added) || !slices.Equal(p.Removed, w.removed) || !slices.Equal(p.Changed, w.changed) {
			t.Errorf("revision %d: %+v", i+1, p)
		}
		for _, k := range p.Added {
			if p.Rules[k] == nil {
				t.Errorf("revision %d lacks rule %s", i+1, k)
			}
		}
	}
	if pol[1].Trace != "trace1" || pol[1].Annotations[policy.AnnotationBy] != policy.ByPolicyAllow || pol[2].Chunk != "c1" {
		t.Errorf("attribution: %+v %+v", pol[1], pol[2])
	}
	req := kinds(recs, KindRequest)
	if req[0].Verdict != VerdictAllow || req[0].Host != "pypi.org" || req[0].Binary != "/usr/bin/pip" || req[0].Rule != "pypi" ||
		req[1].Verdict != VerdictDeny || req[1].Reason == "" || req[1].Sandbox != "webapp" {
		t.Errorf("requests: %+v", req)
	}
	act := kinds(recs, KindAction)
	if act[0].Command != "brig policy allow dev webapp api.github.com" || act[0].Status != "ok" || act[0].Trace == "" {
		t.Errorf("actions: %+v", act)
	}
	drafts := kinds(recs, KindDraft)
	if drafts[0].Chunk == drafts[1].Chunk || !slices.Contains([]string{drafts[0].Endpoints[0], drafts[1].Endpoints[0]}, "cdn.example.com:443") {
		t.Errorf("drafts: %+v", drafts)
	}

	x, err := s.ReadIndex("dev")
	if err != nil {
		t.Fatal(err)
	}
	if x.Collector.Started.IsZero() || x.Collector.Stopped.IsZero() || x.Sandboxes["default/webapp"] != (time.Time{}) {
		t.Errorf("index: %+v", x)
	}
	var pos positions
	if err := s.ReadPositions("dev", &pos); err != nil {
		t.Fatal(err)
	}
	if pos.Logs["default/webapp"].Cursor != "2" || pos.Clean || pos.Sandboxes["default/webapp"].Policy != 4 {
		t.Errorf("positions: %+v", pos)
	}

	// Restarted, the collector resumes at the cursor, records the draft
	// decided meanwhile and the sandbox deleted, and nothing twice.
	g.mu.Lock()
	g.drafts[1].Status, g.drafts[1].Decided = "rejected", time.Now()
	g.mu.Unlock()
	collect(t, s, g, activityLog, func(recs []Record) bool { return len(kinds(recs, KindDraft)) == 3 })
	g.mu.Lock()
	if last := g.watches[len(g.watches)-1]; last.Cursor != "2" {
		t.Errorf("resumed at %+v", last)
	}
	g.sandboxes, g.down = nil, true
	g.mu.Unlock()
	recs = collect(t, s, g, activityLog, func(recs []Record) bool { return len(kinds(recs, KindSandbox)) == 2 })
	if len(kinds(recs, KindPolicy)) != 4 || len(kinds(recs, KindAction)) != 1 || len(kinds(recs, KindGap)) != 0 {
		t.Errorf("records after restarts: %+v", recs)
	}
	pos = positions{}
	if err := s.ReadPositions("dev", &pos); err != nil {
		t.Fatal(err)
	}
	if !pos.Clean || len(pos.Sandboxes) != 0 {
		t.Errorf("positions after the gateway went: %+v", pos)
	}
	if x, _ := s.ReadIndex("dev"); x.Sandboxes["default/webapp"].IsZero() {
		t.Errorf("deleted sandbox not marked: %+v", x.Sandboxes)
	}
}

func TestCollectorRefusesASecond(t *testing.T) {
	s := Store{Root: t.TempDir()}
	w, err := s.OpenWriter("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close(nil) }()
	c := &Collector{VM: "dev", Store: s, Dial: func() (Gateway, error) { return newFakeGateway(), nil }}
	if err := c.Run(context.Background()); !errors.Is(err, ErrCollecting) {
		t.Errorf("second collector: %v", err)
	}
}

func TestCollectorNotesAnOutage(t *testing.T) {
	s := Store{Root: t.TempDir()}
	w, err := s.OpenWriter("dev")
	if err != nil {
		t.Fatal(err)
	}
	w.Now = func() time.Time { return time.Now().Add(-3 * time.Hour) }
	if err := w.Flush(positions{}); err != nil {
		t.Fatal(err)
	}
	w.unlock()
	g := newFakeGateway()
	recs := collect(t, s, g, "", func(recs []Record) bool { return len(kinds(recs, KindGap)) == 1 })
	if gap := kinds(recs, KindGap)[0]; gap.Sandbox != "" || !strings.Contains(gap.Why, "did not run since") {
		t.Errorf("gap: %+v", gap)
	}
}
