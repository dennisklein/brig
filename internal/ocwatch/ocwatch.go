// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package ocwatch follows the logs of an OpenShell gateway's sandboxes for
// the requests that their proxies decided: one watch per sandbox, resumed
// where the last one ended, with a gap reported wherever lines may be
// missing. brig ui shows what it reads; the audit collector writes it down.
package ocwatch

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// Backfill is how far back a watch from the tail of a sandbox's log
	// reaches by default.
	Backfill = time.Hour
	// TailLines is how many lines a watch from the tail asks for: the
	// gateway's default buffer per sandbox.
	TailLines = 2000
	// maxBackoff is the longest wait before a watch starts again.
	maxBackoff = 30 * time.Second
)

// Key identifies a sandbox of a gateway: names are unique per workspace
// only.
type Key struct{ Workspace, Name string }

// String writes the key as WORKSPACE/NAME.
func (k Key) String() string { return k.Workspace + "/" + k.Name }

// Position is where a reader is in a sandbox's log.
type Position struct {
	// Cursor resumes after the last event read, as long as the gateway
	// still has it.
	Cursor string `json:"cursor,omitempty"`
	// Last is the time of the last line read.
	Last time.Time `json:"last,omitzero"`
}

// Client follows a sandbox's log; osclient.Client is one.
type Client interface {
	Watch(ctx context.Context, workspace, sandbox string, from osclient.WatchFrom, each func(osclient.WatchEvent)) error
}

// Watcher keeps one watch per sandbox it is told to follow. Its positions
// outlive a connection to the gateway, so that a watch on a new connection
// resumes where the last one ended.
type Watcher struct {
	// Who names the reader in gaps and log lines, such as "brig ui".
	Who string
	// Backfill is how far back a watch from the tail reaches; zero is
	// Backfill.
	Backfill time.Duration
	// Backoff is the first wait before a failed watch starts again; zero
	// is a second.
	Backoff time.Duration
	// Request is called with each request read, Gap with each place where
	// requests may be missing. Both are called from the watches'
	// goroutines.
	Request func(k Key, r store.Request)
	Gap     func(k Key, at time.Time, why string)
	// Log tells why a sandbox's log cannot be read; nil logs nothing.
	Log *slog.Logger

	mu        sync.Mutex
	positions map[Key]*Position
	watching  map[Key]context.CancelFunc
	wg        sync.WaitGroup
}

// SetPositions sets where the watches started from now on resume.
func (w *Watcher) SetPositions(ps map[Key]Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.positions = map[Key]*Position{}
	for k, p := range ps {
		w.positions[k] = &p
	}
}

// Positions returns where each followed sandbox's watch is.
func (w *Watcher) Positions() map[Key]Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[Key]Position, len(w.positions))
	for k, p := range w.positions {
		out[k] = *p
	}
	return out
}

// Follow starts a watch through c for each sandbox of keys that has none,
// and stops those of sandboxes that keys lacks, forgetting their
// positions. The watches end with ctx.
func (w *Watcher) Follow(ctx context.Context, c Client, keys []Key) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.positions == nil {
		w.positions = map[Key]*Position{}
	}
	if w.watching == nil {
		w.watching = map[Key]context.CancelFunc{}
	}
	listed := map[Key]bool{}
	for _, k := range keys {
		listed[k] = true
		if _, ok := w.watching[k]; ok {
			continue
		}
		pos := w.positions[k]
		if pos == nil {
			pos = &Position{}
			w.positions[k] = pos
		}
		wctx, cancel := context.WithCancel(ctx)
		w.watching[k] = cancel
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.watch(wctx, c, k, pos)
			w.mu.Lock()
			delete(w.watching, k)
			w.mu.Unlock()
			cancel()
		}()
	}
	for k, cancel := range w.watching {
		if !listed[k] {
			cancel()
		}
	}
	maps.DeleteFunc(w.positions, func(k Key, _ *Position) bool { return !listed[k] })
}

// Wait waits until the watches have ended, as they do with the context
// that Follow was given.
func (w *Watcher) Wait() { w.wg.Wait() }

// watch follows a sandbox's log until ctx ends or the sandbox is gone,
// starting again after an error.
func (w *Watcher) watch(ctx context.Context, c Client, k Key, pos *Position) {
	backoff := w.backoff()
	// logged is the last error logged.
	logged := ""
	for {
		w.mu.Lock()
		from := osclient.WatchFrom{Cursor: pos.Cursor}
		last := pos.Last
		w.mu.Unlock()
		if from.Cursor == "" {
			from.Since, from.Tail = time.Now().Add(-cmp.Or(w.Backfill, Backfill)), TailLines
			if last.After(from.Since) {
				// The lines up to last were read already.
				from.Since = last.Add(time.Millisecond)
			}
		}
		read := false
		err := c.Watch(ctx, k.Workspace, k.Name, from, func(e osclient.WatchEvent) {
			read = true
			w.add(k, pos, e)
		})
		if ctx.Err() != nil {
			return
		}
		if msg := errMessage(err); msg != "" && msg != logged {
			// Once for each new error, not for every try.
			w.log().Warn(w.Who+" cannot follow a sandbox's log", "workspace", k.Workspace, "sandbox", k.Name, "err", msg)
			logged = msg
		}
		switch {
		case errors.Is(err, osclient.ErrCursorLost):
			w.mu.Lock()
			pos.Cursor = ""
			w.mu.Unlock()
			w.gap(k, time.Now(), w.Who+" lost its place in the log, as when the gateway restarts; requests may be missing here.")
			continue
		case status.Code(err) == codes.NotFound:
			// Deleted; the next Follow starts a watch if it comes back.
			return
		}
		if read {
			backoff = w.backoff()
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
func (w *Watcher) add(k Key, pos *Position, e osclient.WatchEvent) {
	if e.Warning != "" {
		w.gap(k, time.Now(), "The gateway dropped lines of the log: "+e.Warning)
		return
	}
	l := e.Line
	w.mu.Lock()
	if e.Cursor != "" {
		pos.Cursor = e.Cursor
	}
	if l != nil && !l.Time.IsZero() {
		pos.Last = l.Time
	}
	w.mu.Unlock()
	if l == nil {
		return
	}
	r, ok := Parse(*l)
	if !ok {
		return
	}
	r.Workspace, r.Sandbox = k.Workspace, k.Name
	if w.Request != nil {
		w.Request(k, r)
	}
}

// Parse reads a line of a sandbox's log as a request, as ParseRequest does,
// and reports whether it is one. A line without a time gets the present.
func Parse(l osclient.LogLine) (store.Request, bool) {
	// Only OCSF events tell of requests; the supervisor's other lines are
	// its own.
	if l.Level != "OCSF" && !strings.HasPrefix(l.Message, "NET:") && !strings.HasPrefix(l.Message, "HTTP:") {
		return store.Request{}, false
	}
	r, ok := osclient.ParseRequest(l.Message)
	if !ok {
		return store.Request{}, false
	}
	r.Time = l.Time
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	return r, true
}

func (w *Watcher) gap(k Key, at time.Time, why string) {
	if w.Gap != nil {
		w.Gap(k, at, why)
	}
}

func (w *Watcher) backoff() time.Duration { return cmp.Or(w.Backoff, time.Second) }

func (w *Watcher) log() *slog.Logger {
	if w.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return w.Log
}

// errMessage is what a watch's error says, or "" for none: a stream the
// gateway ended as streams end is no error.
func errMessage(err error) string {
	if err == nil || errors.Is(err, io.EOF) {
		return ""
	}
	return err.Error()
}
