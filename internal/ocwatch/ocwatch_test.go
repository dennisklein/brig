// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocwatch

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
)

var webapp = Key{Workspace: "default", Name: "webapp"}

type watchFails struct{ tries atomic.Int32 }

func (c *watchFails) Watch(context.Context, string, string, osclient.WatchFrom, func(osclient.WatchEvent)) error {
	c.tries.Add(1)
	return status.Error(codes.PermissionDenied, "permission denied")
}

func TestLogsWhyItCannotWatch(t *testing.T) {
	var buf bytes.Buffer
	c := &watchFails{}
	w := &Watcher{Who: "brig ui", Backoff: time.Millisecond, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	w.Follow(ctx, c, []Key{webapp})
	for c.tries.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	w.Wait()
	// Once, however often it tried.
	if got := strings.Count(buf.String(), "brig ui cannot follow"); got != 1 || !strings.Contains(buf.String(), "sandbox=webapp") || !strings.Contains(buf.String(), "workspace=default") ||
		!strings.Contains(buf.String(), "permission denied") {
		t.Errorf("log:\n%s", buf.String())
	}
}

// script sends its events to the first watch, then fails later ones that
// resume at a cursor with err.
type script struct {
	mu      sync.Mutex
	events  []osclient.WatchEvent
	err     error
	watches []osclient.WatchFrom
}

func (c *script) Watch(ctx context.Context, _, _ string, from osclient.WatchFrom, each func(osclient.WatchEvent)) error {
	c.mu.Lock()
	c.watches = append(c.watches, from)
	first := len(c.watches) == 1
	c.mu.Unlock()
	if !first {
		if c.err != nil && from.Cursor != "" {
			return c.err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	for _, e := range c.events {
		each(e)
	}
	return status.Error(codes.Unavailable, "gone")
}

func TestWatcherReadsRequestsAndGaps(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute).Round(time.Millisecond)
	c := &script{
		events: []osclient.WatchEvent{
			{Cursor: "1", Line: &osclient.LogLine{Time: at, Level: "OCSF", Message: "NET:OPEN [MED] DENIED /usr/bin/node(7) -> registry.npmjs.org:443"}},
			{Cursor: "2", Line: &osclient.LogLine{Time: at.Add(time.Second), Level: "INFO", Message: "policy reloaded"}},
			{Warning: "3 lines dropped"},
		},
		err: osclient.ErrCursorLost,
	}
	var (
		mu   sync.Mutex
		reqs []store.Request
		gaps []string
	)
	w := &Watcher{
		Who: "the audit collector", Backoff: time.Millisecond,
		Request: func(_ Key, r store.Request) { mu.Lock(); reqs = append(reqs, r); mu.Unlock() },
		Gap:     func(_ Key, _ time.Time, why string) { mu.Lock(); gaps = append(gaps, why); mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Follow(ctx, c, []Key{webapp})
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.watches)
		c.mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	w.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 1 || reqs[0].Host != "registry.npmjs.org" || !reqs[0].Denied() || reqs[0].Sandbox != "webapp" || !reqs[0].Time.Equal(at) {
		t.Errorf("requests: %+v", reqs)
	}
	if len(gaps) != 2 || !strings.Contains(gaps[0], "3 lines dropped") || !strings.HasPrefix(gaps[1], "the audit collector lost its place") {
		t.Errorf("gaps: %q", gaps)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The second watch resumes at the cursor; the third, after the cursor
	// was lost, after the last line read.
	if c.watches[1].Cursor != "2" || c.watches[2].Cursor != "" || !c.watches[2].Since.Equal(at.Add(time.Second+time.Millisecond)) {
		t.Errorf("watches: %+v", c.watches)
	}
}

func TestPositionsSurviveAndGo(t *testing.T) {
	w := &Watcher{}
	w.SetPositions(map[Key]Position{webapp: {Cursor: "7"}, {Workspace: "default", Name: "gone"}: {Cursor: "1"}})
	ctx, cancel := context.WithCancel(context.Background())
	c := &script{err: nil}
	c.watches = []osclient.WatchFrom{{}} // not the first watch: it blocks
	w.Follow(ctx, c, []Key{webapp})
	cancel()
	w.Wait()
	ps := w.Positions()
	if len(ps) != 1 || ps[webapp].Cursor != "7" {
		t.Errorf("positions: %+v", ps)
	}
	if c.watches[1].Cursor != "7" {
		t.Errorf("watch: %+v", c.watches[1])
	}
}
