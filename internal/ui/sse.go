// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

const (
	// coalesceEvery is how long changes gather before a page gets them,
	// so that a burst of changes costs one render.
	coalesceEvery = 250 * time.Millisecond
	// keepAliveEvery is how often an idle stream sends a comment, so that
	// nothing between drops it and a gone browser is noticed.
	keepAliveEvery = 25 * time.Second
)

// events streams a page's live updates as Server-Sent Events: each event
// is named after a fragment and carries its HTML, which htmx's SSE
// extension swaps into the element with that sse-swap name. The query
// names the fragments (f) and what they need: the VM (vm) and its tab (tab),
// or what liveOpenShell reads.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	names := q["f"]
	if len(names) == 0 {
		http.Error(w, "no fragments named", http.StatusBadRequest)
		return
	}
	for _, n := range names {
		if _, ok := fragments[n]; !ok {
			http.Error(w, "unknown fragment", http.StatusBadRequest)
			return
		}
	}
	vmName := q.Get("vm")
	if vmName != "" && vm.ValidateName(vmName) != nil {
		http.Error(w, "bad VM name", http.StatusBadRequest)
		return
	}
	if !checkOpenShellQuery(q) {
		http.Error(w, "bad gateway or sandbox name", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	changes, unsubscribe := s.st.Subscribe()
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// The browser reconnects after this long when the stream breaks.
	_, _ = io.WriteString(w, "retry: 2000\n\n")
	flusher.Flush()

	csrf := csrfFrom(r.Context())
	pending := map[store.Topic]bool{}
	var timer <-chan time.Time
	keepAlive := time.NewTicker(keepAliveEvery)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case t := <-changes:
			pending[t] = true
			if timer == nil {
				timer = time.After(coalesceEvery)
			}
			continue
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		case <-timer:
		}
		timer = nil
		v := &view{Snap: s.st.Snapshot(), Now: time.Now(), Demo: s.demo, CSRF: csrf, Asset: s.assets.url, Tab: tab(q.Get("tab"))}
		if vmName != "" {
			v.VM, _ = v.Snap.VM(vmName)
		}
		liveOpenShell(v, q)
		for _, n := range names {
			if !slices.ContainsFunc(fragments[n], func(t store.Topic) bool { return pending[t] }) {
				continue
			}
			html, err := s.tmpl.fragment(n, v)
			if err != nil {
				s.log.Error("rendering fragment", "fragment", n, "err", err)
				continue
			}
			if err := writeEvent(w, n, html); err != nil {
				return
			}
		}
		clear(pending)
		flusher.Flush()
	}
}

// writeEvent writes one Server-Sent Event, a data line for each line of
// the HTML in data. A carriage return also ends a line of the stream, so it
// is written as a character reference, which HTML reads the same.
func writeEvent(w io.Writer, name, data string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "event: %s\n", name)
	for _, line := range strings.Split(strings.ReplaceAll(data, "\r", "&#13;"), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}
