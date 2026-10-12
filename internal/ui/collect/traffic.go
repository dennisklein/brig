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
	positions map[string]*position
	// watching holds the cancel function of each sandbox's watch.
	watching map[string]context.CancelFunc
	pending  []store.Request
	// denied is when a sandbox last had a denial whose drafts were not
	// read since.
	denied map[string]time.Time
	// draftsRead is when each sandbox's drafts were last read.
	draftsRead map[string]time.Time
	// draftsErr is the error logged last for each sandbox's drafts.
	draftsErr map[string]string
	// noDrafts is set when the gateway does not offer drafts.
	noDrafts bool
	wg       sync.WaitGroup
}

func newTraffic(g *Gateways, st *store.Store, vm string, c GatewayClient, positions map[string]*position) *traffic {
	return &traffic{
		g: g, st: st, vm: vm, c: c, positions: positions,
		watching: map[string]context.CancelFunc{}, denied: map[string]time.Time{}, draftsRead: map[string]time.Time{},
		draftsErr: map[string]string{},
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
	listed := map[string]bool{}
	for _, s := range sandboxes {
		listed[s.Name] = true
		if _, ok := t.watching[s.Name]; ok {
			continue
		}
		pos := t.positions[s.Name]
		if pos == nil {
			pos = &position{}
			t.positions[s.Name] = pos
		}
		wctx, cancel := context.WithCancel(ctx)
		t.watching[s.Name] = cancel
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.watch(wctx, s.Name, pos)
			t.mu.Lock()
			delete(t.watching, s.Name)
			t.mu.Unlock()
			cancel()
		}()
	}
	for name, cancel := range t.watching {
		if !listed[name] {
			cancel()
		}
	}
	for name := range t.positions {
		if !listed[name] {
			delete(t.positions, name)
		}
	}
}

// watch follows a sandbox's log until ctx ends or the sandbox is gone,
// starting again after an error.
func (t *traffic) watch(ctx context.Context, sandbox string, pos *position) {
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
		err := t.c.Watch(ctx, sandbox, from, func(e osclient.WatchEvent) {
			read = true
			t.add(sandbox, pos, e)
		})
		if ctx.Err() != nil {
			return
		}
		if msg := errMessage(err); msg != "" && msg != logged {
			// Once for each new error, not for every try.
			t.g.log().Warn("brig ui cannot follow a sandbox's log", "vm", t.vm, "sandbox", sandbox, "err", msg)
			logged = msg
		}
		switch {
		case errors.Is(err, osclient.ErrCursorLost):
			pos.cursor = ""
			t.gap(sandbox, "brig ui lost its place in the log, as when the gateway restarts; requests may be missing here.")
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
func (t *traffic) add(sandbox string, pos *position, e osclient.WatchEvent) {
	if e.Warning != "" {
		t.gap(sandbox, "The gateway dropped lines of the log: "+e.Warning)
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
	r.Sandbox, r.Time = sandbox, l.Time
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	t.mu.Lock()
	t.pending = append(t.pending, r)
	if r.Denied() {
		t.denied[sandbox] = time.Now()
	}
	t.mu.Unlock()
}

// gap adds a row that says requests may be missing.
func (t *traffic) gap(sandbox, why string) {
	t.mu.Lock()
	t.pending = append(t.pending, store.Request{Sandbox: sandbox, Time: time.Now(), Gap: why})
	t.mu.Unlock()
}

// readDrafts reads the drafted rules of the sandboxes that had denials a
// while ago, or whose drafts were not read for long, and drops those of
// sandboxes that are gone.
func (t *traffic) readDrafts(ctx context.Context, sandboxes []store.Sandbox) {
	listed := map[string]bool{}
	now := time.Now()
	for _, s := range sandboxes {
		listed[s.Name] = true
		t.mu.Lock()
		denied, read, off := t.denied[s.Name], t.draftsRead[s.Name], t.noDrafts
		t.mu.Unlock()
		due := now.Sub(read) >= t.g.draftsEvery() || (!denied.IsZero() && now.Sub(denied) >= t.g.draftsAfter())
		if off || !due {
			continue
		}
		drafts, err := t.c.Drafts(ctx, s.Name)
		switch {
		case status.Code(err) == codes.Unimplemented:
			t.mu.Lock()
			t.noDrafts = true
			t.mu.Unlock()
			continue
		case err != nil:
			t.mu.Lock()
			again := t.draftsErr[s.Name] == err.Error()
			t.draftsErr[s.Name] = err.Error()
			t.mu.Unlock()
			if !again {
				t.g.log().Warn("brig ui cannot read the drafted rules of a sandbox", "vm", t.vm, "sandbox", s.Name, "err", err)
			}
			continue
		}
		t.mu.Lock()
		t.draftsRead[s.Name] = now
		delete(t.draftsErr, s.Name)
		if !t.denied[s.Name].After(denied) {
			delete(t.denied, s.Name)
		}
		t.mu.Unlock()
		t.st.SetDrafts(t.vm, s.Name, drafts)
	}
	for _, tr := range t.st.Snapshot().Traffic {
		if tr.VM != t.vm {
			continue
		}
		for _, d := range tr.Drafts {
			if !listed[d.Sandbox] {
				t.st.SetDrafts(t.vm, d.Sandbox, nil)
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
