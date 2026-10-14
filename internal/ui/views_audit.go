// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"html/template"
	"net/http"
	"slices"
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/collect"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// trafficRange is a range of the Traffic page besides the last hour, which
// is live: these come from the audit trail.
type trafficRange struct {
	Key, Label string
	// Back is how far back it reaches, zero for all of the trail.
	Back time.Duration
}

var trafficRanges = []trafficRange{
	{"24h", "24 hours", 24 * time.Hour},
	{"7d", "7 days", 7 * 24 * time.Hour},
	{"all", "All", 0},
}

// validRange reports whether r is one of trafficRanges.
func validRange(r string) bool {
	return slices.ContainsFunc(trafficRanges, func(t trafficRange) bool { return t.Key == r })
}

// trafficHistory is the Traffic page's rows when they come from the audit
// trail.
type trafficHistory struct {
	Rows []trafficRow
	// Total is how many rows the range holds, of which Rows are the newest.
	Total int
	Err   string
}

// historyRows reads the requests, drops and gaps that q picks from the
// audit trail, the newest limit of them, newest first, and how many there
// were.
func historyRows(h audit.Reader, q trafficQuery, now time.Time, limit int) ([]trafficRow, int, error) {
	aq := audit.Query{Workspace: q.Workspace, Sandbox: q.Sandbox, Denied: q.Denied, Kinds: []string{audit.KindRequest, audit.KindGap}}
	if q.dropsShown() {
		aq.Kinds = append(aq.Kinds, audit.KindDrop)
	}
	if q.VM != "" {
		aq.VMs = []string{q.VM}
	}
	for _, r := range trafficRanges {
		if r.Key == q.Range && r.Back > 0 {
			aq.Since = now.Add(-r.Back)
		}
	}
	ring := make([]trafficRow, 0, limit)
	n := 0
	err := h.Read(aq, func(r audit.Record) bool {
		row := trafficRow{Request: requestOf(r)}
		if r.Kind == audit.KindDrop {
			row = dropRow(collect.DropOf(r))
		}
		if len(ring) < limit {
			ring = append(ring, row)
		} else {
			ring[n%limit] = row
		}
		n++
		return true
	})
	if n > limit {
		// Put the ring in order again, oldest first.
		k := n % limit
		ring = append(ring[k:], ring[:k]...)
	}
	slices.Reverse(ring)
	if err != nil {
		return ring, n, err
	}
	drops, err := historyDrops(h, ring)
	markDrops(ring, drops)
	return ring, n, err
}

// requestOf is a record of the audit trail as a row of a traffic table. It
// has no Seq, as the store does not hold it.
func requestOf(r audit.Record) store.Request {
	return store.Request{
		VM: r.VM, Workspace: r.Workspace, Sandbox: r.Sandbox, Time: r.T, Verdict: r.Verdict, Method: r.Method,
		Process: r.Binary, PID: r.PID, Host: r.Host, Port: r.Port, Path: r.Path, Policy: r.Rule, Engine: r.Engine,
		Reason: r.Reason, Raw: r.Raw, Gap: r.Why,
	}
}

// sandboxChoice is a sandbox that the Traffic page can be narrowed to.
type sandboxChoice struct {
	Workspace, Name string
	// Deleted says that only its history is left.
	Deleted bool
}

// trafficSandboxes lists the sandboxes of q's VM: those its gateway has,
// and those deleted whose history is kept.
func trafficSandboxes(sn store.Snapshot, q trafficQuery) []sandboxChoice {
	if q.VM == "" {
		return nil
	}
	var out []sandboxChoice
	seen := map[[2]string]bool{}
	if g, ok := sn.Gateway(q.VM); ok {
		for _, s := range g.Sandboxes {
			if q.Workspace == "" || s.Workspace == q.Workspace {
				out = append(out, sandboxChoice{Workspace: s.Workspace, Name: s.Name})
				seen[[2]string{s.Workspace, s.Name}] = true
			}
		}
	}
	if a, ok := sn.Audit.AuditOf(q.VM); ok {
		for _, p := range a.Sandboxes {
			k := [2]string{p.Workspace(), p.Sandbox()}
			if p.Name == "" || p.Deleted.IsZero() || seen[k] || (q.Workspace != "" && k[0] != q.Workspace) {
				continue
			}
			out = append(out, sandboxChoice{Workspace: k[0], Name: k[1], Deleted: true})
		}
	}
	return out
}

// deletedSandbox reports whether q picks a sandbox that only the audit
// trail still has.
func deletedSandbox(sn store.Snapshot, q trafficQuery) bool {
	for _, c := range trafficSandboxes(sn, q) {
		if c.Workspace == q.Workspace && c.Name == q.Sandbox {
			return c.Deleted
		}
	}
	return false
}

// historyView is what a sandbox's History tab is rendered from.
type historyView struct {
	*view
	VMName, Workspace, Name string
	// Records are the sandbox's policy, draft and sandbox records, newest
	// first.
	Records []audit.Record
	// Reach is what it reached in the last ReachDays days.
	Reach     []audit.Reach
	ReachDays int
	Err       string
}

// historyReachDays is how far back a History tab sums up what a sandbox
// reached.
const historyReachDays = 7

// sandboxHistory renders a sandbox's History tab, for a sandbox that is
// gone too.
func (s *Server) sandboxHistory(w http.ResponseWriter, r *http.Request) {
	vmName, ws, name := r.PathValue("vm"), r.PathValue("workspace"), r.PathValue("name")
	if vm.ValidateName(vmName) != nil || !action.ValidWorkspaceName(ws) || !action.ValidSandboxName(name) {
		s.notFound(w, r)
		return
	}
	h := &historyView{view: s.newView(r, "", ""), VMName: vmName, Workspace: ws, Name: name, ReachDays: historyReachDays}
	if s.history == nil {
		h.Err = "brig ui reads no audit trail here."
		s.fragmentResponse(w, http.StatusOK, "sandbox-history", h)
		return
	}
	q := audit.Query{VMs: []string{vmName}, Workspace: ws, Sandbox: name, Kinds: []string{audit.KindPolicy, audit.KindDraft, audit.KindSandbox}}
	err := s.history.Read(q, func(rec audit.Record) bool { h.Records = append(h.Records, rec); return true })
	if err == nil {
		slices.Reverse(h.Records)
		q.Kinds, q.Since = nil, clock().AddDate(0, 0, -historyReachDays)
		h.Reach, err = audit.Summarize(s.history, q)
	}
	if err != nil {
		s.log.Warn("reading the audit trail", "vm", vmName, "workspace", ws, "sandbox", name, "err", err)
		h.Err = "brig ui could not read the audit trail: " + err.Error()
	}
	s.fragmentResponse(w, http.StatusOK, "sandbox-history", h)
}

// auditNeeds lists the running VMs whose traffic nothing records.
func auditNeeds(sn store.Snapshot) []need {
	var out []need
	for _, a := range sn.Audit.VMs {
		if !a.Problem {
			continue
		}
		detail := "Its collector, " + audit.UnitName(a.VM) + ", is " + a.Collector + ". brig start " + a.VM + " starts it again."
		if a.Error != "" {
			detail = a.Error
		}
		out = append(out, need{Sev: "warn", Title: "Nothing records the traffic of " + a.VM, Detail: detail, Link: "/host", Action: "Show"})
	}
	return out
}

// auditFuncs returns the template functions of the audit trail's views.
func auditFuncs() template.FuncMap {
	return template.FuncMap{
		"describe":         audit.Describe,
		"trafficRanges":    func() []trafficRange { return trafficRanges },
		"trafficSandboxes": func(v *view) []sandboxChoice { return trafficSandboxes(v.Snap, v.TrafficQ) },
		"deletedSandbox":   func(v *view) bool { return deletedSandbox(v.Snap, v.TrafficQ) },
		"pruneAges":        func() []string { return action.PruneAges },
		"disk":             func(n int64) string { return fileSize(max(n, 0)) },
		"auditOf": func(sn store.Snapshot, vm string) store.AuditVM {
			a, _ := sn.Audit.AuditOf(vm)
			return a
		},
		"hasVM": func(sn store.Snapshot, name string) bool {
			_, ok := sn.VM(name)
			return ok
		},
		// share is n as a percentage of total, for the width of a bar.
		"share": func(n, total int64) int64 {
			if total <= 0 {
				return 0
			}
			return max(1, min(100, n*100/total))
		},
		"pruneAct": func(v *view, vmName string) actButton {
			return act(v, "audit-prune", "", "sm", "vm", vmName)
		},
		"auditCollector": func(a store.AuditVM) string {
			if a.Collector == audit.StateVMDeleted && !a.Deleted.IsZero() {
				return a.Collector + " " + a.Deleted.Local().Format("2 Jan")
			}
			return a.Collector
		},
	}
}
