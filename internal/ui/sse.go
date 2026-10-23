// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"slices"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb/session"
	"github.com/GSI-HPC/go-clikit/localweb/sse"

	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// coalesceEvery is how long changes gather before a page gets them, so
// that a burst of changes costs one render. The kit's default is 30 ms.
const coalesceEvery = 250 * time.Millisecond

// events streams a page's live updates as Server-Sent Events: each event
// is named after a fragment and carries its HTML, which htmx's SSE
// extension swaps into the element with that sse-swap name. The query
// names the fragments (f) and what they need: the VM (vm) and its tab (tab),
// or what liveOpenShell reads.
//
// The stream opens with every fragment, as the page may have been rendered
// before a change that the stream does not hear of. Then changes gather for
// coalesceEvery, and each fragment that depends on a changed topic is
// rendered and sent, unless its HTML is what the stream sent last.
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
	csrf := session.CSRF(r.Context())
	// render renders the fragments that depend on a topic of changed, or
	// all of them when changed is nil, from the current snapshot.
	render := func(changed []store.Topic) ([]sse.Event, error) {
		v := &view{Snap: s.st.Snapshot(), Now: clock(), Demo: s.demo, CSRF: csrf, Tab: tab(q.Get("tab"))}
		if vmName != "" {
			v.VM, _ = v.Snap.VM(vmName)
		}
		liveOpenShell(v, q)
		var events []sse.Event
		for _, n := range names {
			if changed != nil && !slices.ContainsFunc(fragments[n], func(t store.Topic) bool { return slices.Contains(changed, t) }) {
				continue
			}
			html, err := s.tmpl.fragment(n, v)
			if err != nil {
				s.log.Error("rendering fragment", "fragment", n, "err", err)
				continue
			}
			events = append(events, sse.Event{Name: n, Data: html})
		}
		return events, nil
	}
	if err := sse.Serve(w, r, s.st.Notifier(), render, sse.Options{Coalesce: coalesceEvery, Done: s.ctx.Done()}); err != nil {
		s.log.Error("streaming live updates", "err", err)
	}
}
