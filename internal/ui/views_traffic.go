// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"cmp"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/fence2"
	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// trafficShown is how many rows a traffic table shows, newest first.
	trafficShown = 200
	// recentWindow is how far back denials count as recent: for Needs you,
	// the rail and Topology.
	recentWindow = time.Hour
)

// trafficQuery picks the requests that a traffic table shows.
type trafficQuery struct {
	// VM narrows it to a VM's gateway, Workspace to a workspace, and
	// Sandbox to one sandbox of that workspace.
	VM, Workspace, Sandbox string
	// Denied shows denied requests only, and the rows brig ui could not
	// read.
	Denied bool
	// Range is one of trafficRanges, read from the audit trail, or empty
	// for the last hour, live.
	Range string
}

// URL is the Traffic page that shows q.
func (q trafficQuery) URL() string {
	v := url.Values{}
	if q.Denied {
		v.Set("verdict", "deny")
	}
	if q.VM != "" {
		v.Set("vm", q.VM)
	}
	if q.Workspace != "" {
		v.Set("workspace", q.Workspace)
	}
	if q.Sandbox != "" {
		v.Set("sandbox", q.Sandbox)
	}
	if q.Range != "" {
		v.Set("range", q.Range)
	}
	if len(v) == 0 {
		return "/traffic"
	}
	return "/traffic?" + v.Encode()
}

// With returns q with the verdict filter set to denied.
func (q trafficQuery) With(denied bool) trafficQuery {
	q.Denied = denied
	return q
}

// OnVM returns q for the VM named vm, any sandbox.
func (q trafficQuery) OnVM(vm string) trafficQuery {
	q.VM, q.Sandbox = vm, ""
	return q
}

// OnWorkspace returns q for the workspace ws, any sandbox.
func (q trafficQuery) OnWorkspace(ws string) trafficQuery {
	q.Workspace, q.Sandbox = ws, ""
	return q
}

// OnRange returns q for the range r.
func (q trafficQuery) OnRange(r string) trafficQuery {
	q.Range = r
	return q
}

// OnSandbox returns q for the sandbox name of workspace ws, or for any
// sandbox with an empty name. A deleted sandbox has history only, so the
// live range would show it empty.
func (q trafficQuery) OnSandbox(ws, name string, deleted bool) trafficQuery {
	q.Sandbox = name
	if name != "" {
		q.Workspace = ws
	}
	if deleted && q.Range == "" {
		q.Range = "all"
	}
	return q
}

// trafficRow is a row of a traffic table: a request, or what fence 2
// dropped.
type trafficRow struct {
	store.Request
	// Draft is the pending rule that OpenShell drafted and that would
	// allow the request, if there is exactly one.
	Draft *store.Draft
	// Drop is what fence 2 dropped, for a drop row; its Request holds
	// the VM and time only.
	Drop *store.Drop
	// Dropped is the drop that likely stopped an allowed request.
	Dropped *store.Drop
}

// dropRow is a drop as a row of a traffic table.
func dropRow(d store.Drop) trafficRow {
	return trafficRow{Request: store.Request{VM: d.VM, Time: d.Time}, Drop: &d}
}

// Unparsed reports whether the row is a line that brig ui could not read.
func (r trafficRow) Unparsed() bool { return r.Raw != "" }

// What says what the request was, such as "GET registry.npmjs.org:443/x".
func (r trafficRow) What() string {
	if r.Method == "" {
		return r.Endpoint()
	}
	return r.Method + " " + r.Endpoint() + r.Path
}

// programRE matches the name of a program that a path check may name.
var programRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// NameOnly reports whether the log named the program without its path, by a
// plain name.
func (r trafficRow) NameOnly() bool { return programRE.MatchString(r.Process) }

// Who names the program that made the request.
func (r trafficRow) Who() string {
	return cmp.Or(r.Process, "an unknown program")
}

// dropsShown reports whether q's table shows drops: they are the VM's, not
// a sandbox's.
func (q trafficQuery) dropsShown() bool { return q.Workspace == "" && q.Sandbox == "" }

// matches reports whether the request belongs in q's table.
func (q trafficQuery) matches(r store.Request) bool {
	switch {
	case q.Workspace != "" && r.Workspace != q.Workspace:
		return false
	case q.Sandbox != "" && r.Sandbox != q.Sandbox:
		return false
	case q.Denied:
		return r.Denied() || r.Raw != "" || r.Gap != ""
	}
	return true
}

// trafficRows returns the newest requests that q picks, at most limit,
// newest first.
func trafficRows(sn store.Snapshot, q trafficQuery, limit int) []trafficRow {
	var out []trafficRow
	for _, t := range sn.Traffic {
		if q.VM != "" && t.VM != q.VM {
			continue
		}
		n := 0
		for i := len(t.Requests) - 1; i >= 0 && n < limit; i-- {
			r := t.Requests[i]
			if !q.matches(r) {
				continue
			}
			row := trafficRow{Request: r}
			if r.Denied() {
				if d, ok := t.DraftFor(r); ok {
					row.Draft = &d
				}
			}
			out = append(out, row)
			n++
		}
		if q.dropsShown() {
			for i := len(t.Drops) - 1; i >= 0 && i >= len(t.Drops)-limit; i-- {
				out = append(out, dropRow(t.Drops[i]))
			}
		}
	}
	slices.SortStableFunc(out, func(a, b trafficRow) int { return b.Time.Compare(a.Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	markDrops(out, sn.TrafficOf)
	return out
}

// denialCount counts the denials since the time since, of the VM named vm
// and its sandbox in the workspace ws, or of all when empty.
func denialCount(sn store.Snapshot, vm, ws, sandbox string, since time.Time) int {
	n := 0
	for _, t := range sn.Traffic {
		if vm != "" && t.VM != vm {
			continue
		}
		for i := len(t.Requests) - 1; i >= 0 && !t.Requests[i].Time.Before(since); i-- {
			if r := t.Requests[i]; r.Denied() && (sandbox == "" || (r.Workspace == ws && r.Sandbox == sandbox)) {
				n++
			}
		}
	}
	return n
}

// oldestShown returns the time of the oldest request a gateway's buffer
// gave brig ui, of the VMs that q picks: older traffic is gone.
func oldestShown(sn store.Snapshot, q trafficQuery) time.Time {
	var oldest time.Time
	for _, t := range sn.Traffic {
		if (q.VM == "" || t.VM == q.VM) && len(t.Requests) > 0 {
			if first := t.Requests[0].Time; oldest.IsZero() || first.Before(oldest) {
				oldest = first
			}
		}
	}
	return oldest
}

// allowAct is the button that opens Allow… for a denied request.
func allowAct(v *view, r trafficRow) actButton {
	return act(v, "allow", "", "sm", "vm", r.VM, "workspace", r.Workspace, "sandbox", r.Sandbox, "req", strconv.FormatUint(r.Seq, 10))
}

// trafficNeeds lists the sandboxes with denials in the last hour, the
// latest denial first.
func trafficNeeds(sn store.Snapshot) []need {
	type key struct{ vm, workspace, sandbox string }
	counts := map[key]int{}
	latest := map[key]store.Request{}
	since := clock().Add(-recentWindow)
	for _, t := range sn.Traffic {
		for i := len(t.Requests) - 1; i >= 0 && !t.Requests[i].Time.Before(since); i-- {
			r := t.Requests[i]
			if !r.Denied() {
				continue
			}
			k := key{t.VM, r.Workspace, r.Sandbox}
			if counts[k] == 0 {
				latest[k] = r
			}
			counts[k]++
		}
	}
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int { return latest[b].Time.Compare(latest[a].Time) })
	var out []need
	for _, k := range keys {
		n, r := counts[k], trafficRow{Request: latest[k]}
		title := fmt.Sprintf("%d denied request%s from %s on %s in the last hour", n, plural(n), sandboxLabel(sn, k.vm, k.workspace, k.sandbox), k.vm)
		detail := "Latest: " + r.What() + " by " + r.Who()
		if r.Reason != "" {
			detail += ": " + r.Reason
		}
		out = append(out, need{
			Sev: "warn", Title: title, Detail: detail,
			Link:   trafficQuery{VM: k.vm, Workspace: k.workspace, Sandbox: k.sandbox, Denied: true}.URL(),
			Dialog: dialogURL("allow", "vm", k.vm, "workspace", k.workspace, "sandbox", k.sandbox, "req", strconv.FormatUint(r.Seq, 10)),
			Action: "Allow…",
		})
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Request returns the request that the dialog's field "req" names, if the
// store still holds it.
func (d *dialog) Request() *trafficRow {
	seq, err := strconv.ParseUint(d.Form.Get("req"), 10, 64)
	if err != nil {
		return nil
	}
	t := d.Snap.TrafficOf(d.Form.Get("vm"))
	r, ok := t.Request(seq)
	if !ok || r.Workspace != d.Form.Get("workspace") || r.Sandbox != d.Form.Get("sandbox") {
		return nil
	}
	row := trafficRow{Request: r}
	if dr, ok := t.DraftFor(r); ok {
		row.Draft = &dr
	}
	return &row
}

// PathCheck is the command that prints the full path of the program that
// the request of the dialog named.
func (d *dialog) PathCheck() string {
	r := d.Request()
	if r == nil || !r.NameOnly() {
		return ""
	}
	return oscmd(d.Form.Get("vm"), r.Workspace, "sandbox", "exec", "-n", r.Sandbox, "--no-tty", "--",
		"sh", "-c", `readlink -f "$(command -v `+r.Process+`)"`)
}

// Draft returns the pending draft that the dialog's field "draft" names.
func (d *dialog) Draft() *store.Draft {
	dr, ok := d.Snap.TrafficOf(d.Form.Get("vm")).Draft(d.Form.Get("workspace"), d.Form.Get("sandbox"), d.Form.Get("draft"))
	if !ok {
		return nil
	}
	return &dr
}

// allowDefaults fills the allow dialog from the denied request it was
// opened for, or the draft: the draft if OpenShell made one, else a rule
// for the request's endpoint and program.
func allowDefaults(d *dialog) {
	host, port, binary, access, protocol := "", 0, "", "read-only", "rest"
	if r := d.Request(); r != nil {
		host, port, binary = r.Host, r.Port, r.Process
		if binary != "" && !strings.HasPrefix(binary, "/") {
			// The log named the program only; this is a guess to check.
			binary = "/usr/bin/" + binary
		}
		switch r.Method {
		case "", "GET", "HEAD", "OPTIONS":
		default:
			access = "read-write"
		}
		if r.Method == "" && port != 80 && port != 443 && port != 8080 && port != 8443 {
			protocol = ""
		}
		if r.Draft != nil {
			d.def("draft", r.Draft.ID)
		}
	}
	if dr := d.Draft(); dr != nil {
		if len(dr.Endpoints) > 0 && host == "" {
			host, port = dr.Endpoints[0].Host, dr.Endpoints[0].Port
		}
		binary = cmp.Or(dr.Binary, binary)
		d.def("mode", "draft")
	}
	d.def("mode", "rule")
	d.def("host", host)
	if port != 0 {
		d.def("port", strconv.Itoa(port))
	}
	d.def("binary", binary)
	d.def("access", access)
	d.def("protocol", protocol)
	d.def("enforcement", "enforce")
}

// allowSub is the line below the allow dialog's title: the request it was
// opened for.
func allowSub(d *dialog) string {
	r := d.Request()
	if r == nil {
		return "Adds a network rule to the policy of " + d.Form.Get("sandbox") + " in workspace " + d.Form.Get("workspace") + "."
	}
	s := r.What() + " by " + r.Who() + " was denied"
	if r.Reason != "" {
		s += ": " + r.Reason
	}
	return s + "."
}

// trafficFuncs returns the template functions of the traffic pages.
func trafficFuncs() template.FuncMap {
	return template.FuncMap{
		"trafficShown": func() int { return trafficShown },
		"trafficRows":  func(v *view) []trafficRow { return trafficRows(v.Snap, v.TrafficQ, trafficShown) },
		"sandboxTraffic": func(v *view) []trafficRow {
			return trafficRows(v.Snap, trafficQuery{VM: v.Gateway.VM, Workspace: v.Sandbox.Workspace, Sandbox: v.Sandbox.Name}, trafficShown)
		},
		"sandboxDrafts": func(v *view) []store.Draft {
			var out []store.Draft
			for _, d := range v.Snap.TrafficOf(v.Gateway.VM).Drafts {
				if d.Workspace == v.Sandbox.Workspace && d.Sandbox == v.Sandbox.Name {
					out = append(out, d)
				}
			}
			return out
		},
		"recentDenials": func(v *view) int { return denialCount(v.Snap, "", "", "", clock().Add(-recentWindow)) },
		"sandboxDenials": func(v *view) int {
			return denialCount(v.Snap, v.Gateway.VM, v.Sandbox.Workspace, v.Sandbox.Name, clock().Add(-recentWindow))
		},
		"oldestShown": oldestShown,
		"allowAct":    allowAct,
		"dropAct":     dropAct,
		"dropWhat":    dropWhat,
		"explainDrop": fence2.Explain,
		"draftAct": func(v *view, d store.Draft) actButton {
			return act(v, "allow", "Approve…", "sm", "vm", v.Gateway.VM, "workspace", d.Workspace, "sandbox", d.Sandbox, "draft", d.ID)
		},
		"rejectAct": func(v *view, d store.Draft) actButton {
			return act(v, "reject", "Reject…", "sm", "vm", v.Gateway.VM, "workspace", d.Workspace, "sandbox", d.Sandbox, "draft", d.ID)
		},
		"saveAct": func(v *view) actButton {
			return act(v, "policy-save-preview", "", "sm", "vm", v.Gateway.VM, "workspace", v.Sandbox.Workspace, "sandbox", v.Sandbox.Name)
		},
		"trafficURL": func(vm, ws, sandbox string, denied bool) string {
			return trafficQuery{VM: vm, Workspace: ws, Sandbox: sandbox, Denied: denied}.URL()
		},
		"denialRows": func(v *view, limit int) []trafficRow {
			rows := trafficRows(v.Snap, trafficQuery{Denied: true}, 4*limit)
			rows = slices.DeleteFunc(rows, func(r trafficRow) bool { return !r.Denied() })
			return rows[:min(limit, len(rows))]
		},
		"trafficTable": func(v *view, rows []trafficRow, compact bool) trafficTable {
			return trafficTable{V: v, Rows: rows, Compact: compact}
		},
		"topology": topologyOf,
		"topoDetailView": func(v *view, d *topoDetail) topoDetailView {
			return topoDetailView{V: v, D: d}
		},
	}
}

// trafficTable is what a table of requests is rendered from.
type trafficTable struct {
	V    *view
	Rows []trafficRow
	// Compact leaves out the sandbox, for a sandbox's own table.
	Compact bool
}

// topoDetailView is what the panel below the topology is rendered from.
type topoDetailView struct {
	V *view
	D *topoDetail
}

// trafficPalette lists the jump box's entries for traffic and topology.
func trafficPalette() []paletteItem {
	return []paletteItem{
		{"Traffic", "page", "/traffic"},
		{"Traffic · denied", "page", "/traffic?verdict=deny"},
		{"Topology", "page", "/topology"},
	}
}
