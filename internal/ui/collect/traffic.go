// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/ui/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// backfill is how far back a watch from the tail of a sandbox's log
	// reaches.
	backfill = time.Hour
	// tailLines is how many lines a watch from the tail asks for: the
	// gateway's default buffer per sandbox.
	tailLines = 2000
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
type sandboxKey struct{ workspace, name string }

func keyOf(workspace, name string) sandboxKey { return sandboxKey{workspace, name} }

// position is where brig ui is in a sandbox's log. It outlives a
// connection, so that a watch resumes where the last one ended.
type position struct {
	cursor string
	// last is the time of the last line read.
	last time.Time
}

// traffic follows the logs of a gateway's sandboxes for their requests,
// and reads the rules that OpenShell drafted from the denied ones.
type traffic struct {
	g  *Gateways
	st *store.Store
	vm string
	c  GatewayClient

	mu sync.Mutex
	// positions are kept by the worker across connections.
	positions map[sandboxKey]*position
	// watching holds the cancel function of each sandbox's watch.
	watching map[sandboxKey]context.CancelFunc
	pending  []store.Request
	// denied is when a sandbox last had a denial whose drafts were not
	// read since.
	denied map[sandboxKey]time.Time
	// draftsRead is when each sandbox's drafts were last read.
	draftsRead map[sandboxKey]time.Time
	// draftsErr is the error logged last for each sandbox's drafts.
	draftsErr map[sandboxKey]string
	// noDrafts is set when the gateway does not offer drafts.
	noDrafts bool
	wg       sync.WaitGroup
}

func newTraffic(g *Gateways, st *store.Store, vm string, c GatewayClient, positions map[sandboxKey]*position) *traffic {
	return &traffic{
		g: g, st: st, vm: vm, c: c, positions: positions,
		watching: map[sandboxKey]context.CancelFunc{}, denied: map[sandboxKey]time.Time{}, draftsRead: map[sandboxKey]time.Time{},
		draftsErr: map[sandboxKey]string{},
	}
}

// run adds what the watches read to the store until ctx ends, then stops
// the watches.
func (t *traffic) run(ctx context.Context) {
	tick := time.NewTicker(t.g.flushEvery())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.wg.Wait()
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
	t.mu.Lock()
	defer t.mu.Unlock()
	listed := map[sandboxKey]bool{}
	for _, s := range sandboxes {
		k := keyOf(s.Workspace, s.Name)
		listed[k] = true
		if _, ok := t.watching[k]; ok {
			continue
		}
		pos := t.positions[k]
		if pos == nil {
			pos = &position{}
			t.positions[k] = pos
		}
		wctx, cancel := context.WithCancel(ctx)
		t.watching[k] = cancel
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.watch(wctx, k, pos)
			t.mu.Lock()
			delete(t.watching, k)
			t.mu.Unlock()
			cancel()
		}()
	}
	for k, cancel := range t.watching {
		if !listed[k] {
			cancel()
		}
	}
	for k := range t.positions {
		if !listed[k] {
			delete(t.positions, k)
		}
	}
}

// watch follows a sandbox's log until ctx ends or the sandbox is gone,
// starting again after an error.
func (t *traffic) watch(ctx context.Context, k sandboxKey, pos *position) {
	backoff := t.g.backoff()
	// logged is the last error logged.
	logged := ""
	for {
		from := osclient.WatchFrom{Cursor: pos.cursor}
		if from.Cursor == "" {
			from.Since, from.Tail = time.Now().Add(-backfill), tailLines
			if pos.last.After(from.Since) {
				// The lines up to last are in the store already.
				from.Since = pos.last.Add(time.Millisecond)
			}
		}
		read := false
		err := t.c.Watch(ctx, k.workspace, k.name, from, func(e osclient.WatchEvent) {
			read = true
			t.add(k, pos, e)
		})
		if ctx.Err() != nil {
			return
		}
		if msg := errMessage(err); msg != "" && msg != logged {
			// Once for each new error, not for every try.
			t.g.log().Warn("brig ui cannot follow a sandbox's log", "vm", t.vm, "workspace", k.workspace, "sandbox", k.name, "err", msg)
			logged = msg
		}
		switch {
		case errors.Is(err, osclient.ErrCursorLost):
			pos.cursor = ""
			t.gap(k, "brig ui lost its place in the log, as when the gateway restarts; requests may be missing here.")
			continue
		case status.Code(err) == codes.NotFound:
			// Deleted; the next list starts a watch if it comes back.
			return
		}
		if read {
			backoff = t.g.backoff()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// add reads an event of a sandbox's log.
func (t *traffic) add(k sandboxKey, pos *position, e osclient.WatchEvent) {
	if e.Warning != "" {
		t.gap(k, "The gateway dropped lines of the log: "+e.Warning)
		return
	}
	if e.Cursor != "" {
		pos.cursor = e.Cursor
	}
	l := e.Line
	if l == nil {
		return
	}
	if !l.Time.IsZero() {
		pos.last = l.Time
	}
	// Only OCSF events tell of requests; the supervisor's other lines are
	// its own.
	if l.Level != "OCSF" && !strings.HasPrefix(l.Message, "NET:") && !strings.HasPrefix(l.Message, "HTTP:") {
		return
	}
	r, ok := osclient.ParseRequest(l.Message)
	if !ok {
		return
	}
	r.Workspace, r.Sandbox, r.Time = k.workspace, k.name, l.Time
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	t.mu.Lock()
	t.pending = append(t.pending, r)
	if r.Denied() {
		t.denied[k] = time.Now()
	}
	t.mu.Unlock()
}

// gap adds a row that says requests may be missing.
func (t *traffic) gap(k sandboxKey, why string) {
	t.mu.Lock()
	t.pending = append(t.pending, store.Request{Workspace: k.workspace, Sandbox: k.name, Time: time.Now(), Gap: why})
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

// errMessage is what a watch's error says, or "" for none: a stream the
// gateway ended as streams end is no error.
func errMessage(err error) string {
	if err == nil || errors.Is(err, io.EOF) {
		return ""
	}
	return err.Error()
}
