// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/ocwatch"
	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// flushEvery is how often the requests read are added to the store,
	// so that a burst of lines costs one change.
	flushEvery = 250 * time.Millisecond
	// draftsAfter is how long after a denial its sandbox's drafted rules
	// are read: the supervisor sends denials to the gateway in batches.
	draftsAfter = 3 * time.Second
	// draftsEvery is how often the drafted rules of each sandbox are read
	// anyway.
	draftsEvery = 30 * time.Second
)

// sandboxKey identifies a sandbox of a gateway: names are unique per
// workspace only.
type sandboxKey = ocwatch.Key

func keyOf(workspace, name string) sandboxKey { return sandboxKey{Workspace: workspace, Name: name} }

// traffic follows the logs of a gateway's sandboxes for their requests,
// and reads the rules that OpenShell drafted from the denied ones.
type traffic struct {
	g  *Gateways
	st *store.Store
	vm string
	c  GatewayClient
	// w keeps the watches; the worker keeps it across connections, so that
	// a watch resumes where the last one ended.
	w *ocwatch.Watcher

	mu      sync.Mutex
	pending []store.Request
	// denied is when a sandbox last had a denial whose drafts were not
	// read since.
	denied map[sandboxKey]time.Time
	// draftsRead is when each sandbox's drafts were last read.
	draftsRead map[sandboxKey]time.Time
	// draftsErr is the error logged last for each sandbox's drafts.
	draftsErr map[sandboxKey]string
	// noDrafts is set when the gateway does not offer drafts.
	noDrafts bool
}

// newWatcher returns the watcher of the VM's sandboxes' logs, which outlives
// connections to its gateway.
func (g *Gateways) newWatcher(vm string) *ocwatch.Watcher {
	return &ocwatch.Watcher{Who: "brig ui", Backoff: g.Backoff, Log: g.log().With("vm", vm)}
}

func newTraffic(g *Gateways, st *store.Store, vm string, c GatewayClient, w *ocwatch.Watcher) *traffic {
	t := &traffic{
		g: g, st: st, vm: vm, c: c, w: w,
		denied: map[sandboxKey]time.Time{}, draftsRead: map[sandboxKey]time.Time{},
		draftsErr: map[sandboxKey]string{},
	}
	w.Request = t.add
	w.Gap = t.gap
	return t
}

// run adds what the watches read to the store until ctx ends, then stops
// the watches.
func (t *traffic) run(ctx context.Context) {
	tick := time.NewTicker(t.g.flushEvery())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.w.Wait()
			t.flush()
			return
		case <-tick.C:
			t.flush()
		}
	}
}

func (t *traffic) flush() {
	t.mu.Lock()
	rs := t.pending
	t.pending = nil
	t.mu.Unlock()
	t.st.AddRequests(t.vm, rs)
}

// follow starts a watch for each sandbox that has none, and stops those of
// sandboxes that are gone.
func (t *traffic) follow(ctx context.Context, sandboxes []store.Sandbox) {
	keys := make([]sandboxKey, len(sandboxes))
	for i, s := range sandboxes {
		keys[i] = keyOf(s.Workspace, s.Name)
	}
	t.w.Follow(ctx, t.c, keys)
}

// add takes a request that a watch read.
func (t *traffic) add(k sandboxKey, r store.Request) {
	t.mu.Lock()
	t.pending = append(t.pending, r)
	if r.Denied() {
		t.denied[k] = time.Now()
	}
	t.mu.Unlock()
}

// gap adds a row that says requests may be missing.
func (t *traffic) gap(k sandboxKey, at time.Time, why string) {
	t.mu.Lock()
	t.pending = append(t.pending, store.Request{Workspace: k.Workspace, Sandbox: k.Name, Time: at, Gap: why})
	t.mu.Unlock()
}

// readDrafts reads the drafted rules of the sandboxes that had denials a
// while ago, or whose drafts were not read for long, and drops those of
// sandboxes that are gone.
func (t *traffic) readDrafts(ctx context.Context, sandboxes []store.Sandbox) {
	listed := map[sandboxKey]bool{}
	now := time.Now()
	for _, s := range sandboxes {
		k := keyOf(s.Workspace, s.Name)
		listed[k] = true
		t.mu.Lock()
		denied, read, off := t.denied[k], t.draftsRead[k], t.noDrafts
		t.mu.Unlock()
		due := now.Sub(read) >= t.g.draftsEvery() || (!denied.IsZero() && now.Sub(denied) >= t.g.draftsAfter())
		if off || !due {
			continue
		}
		drafts, err := t.c.Drafts(ctx, s.Workspace, s.Name)
		switch {
		case status.Code(err) == codes.Unimplemented:
			t.mu.Lock()
			t.noDrafts = true
			t.mu.Unlock()
			continue
		case err != nil:
			t.mu.Lock()
			again := t.draftsErr[k] == err.Error()
			t.draftsErr[k] = err.Error()
			t.mu.Unlock()
			if !again {
				t.g.log().Warn("brig ui cannot read the drafted rules of a sandbox", "vm", t.vm, "workspace", s.Workspace, "sandbox", s.Name, "err", err)
			}
			continue
		}
		t.mu.Lock()
		t.draftsRead[k] = now
		delete(t.draftsErr, k)
		if !t.denied[k].After(denied) {
			delete(t.denied, k)
		}
		t.mu.Unlock()
		t.st.SetDrafts(t.vm, s.Workspace, s.Name, drafts)
	}
	for _, tr := range t.st.Snapshot().Traffic {
		if tr.VM != t.vm {
			continue
		}
		for _, d := range tr.Drafts {
			if !listed[keyOf(d.Workspace, d.Sandbox)] {
				t.st.SetDrafts(t.vm, d.Workspace, d.Sandbox, nil)
			}
		}
	}
}
