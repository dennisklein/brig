// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/fence2"
	"github.com/dennisklein/brig/internal/ui/store"
)

// Audit returns the demo's audit trail as of now: dev and review collect,
// airgap is stopped, and scratch was deleted with its history kept.
func Audit(now time.Time) store.Audit {
	const mib = 1 << 20
	days := func(n int, rows, disk int64) []store.AuditPart {
		var out []store.AuditPart
		for i := n; i > 0; i-- {
			out = append(out, store.AuditPart{Name: now.UTC().AddDate(0, 0, -i+1).Format(time.DateOnly), Rows: rows, Disk: disk})
		}
		return out
	}
	vm := func(name, collector string, since time.Duration, n int, rows, disk int64, sandboxes ...store.AuditPart) store.AuditVM {
		v := store.AuditVM{
			VM: name, Collector: collector, LastEvent: now.Add(-2 * time.Second), Days: days(n, rows, disk),
			Rows: int64(n) * rows, Disk: int64(n) * disk, Oldest: now.AddDate(0, 0, -n+1), Sandboxes: sandboxes,
		}
		if collector == audit.UnitRunning {
			v.Since = now.Add(-since)
		} else {
			v.LastEvent = now.Add(-since)
		}
		return v
	}
	dev := vm("dev", audit.UnitRunning, 7*time.Hour+34*time.Minute, 21, 18000, 3*mib,
		store.AuditPart{Name: "default/webapp", Rows: 240000, Disk: 41 * mib},
		store.AuditPart{Name: "default/kit-dev", Rows: 90000, Disk: 15 * mib},
		store.AuditPart{Name: "default/spike", Rows: 30000, Disk: 5 * mib, Deleted: now.Add(-50 * time.Hour)},
		store.AuditPart{Name: "team/webapp", Rows: 18000, Disk: 2 * mib},
		store.AuditPart{Name: "", Rows: 300, Disk: mib / 8},
	)
	dev.Fence2 = string(fence2.Itemised)
	review := vm("review", audit.UnitRunning, 26*time.Hour, 12, 4000, mib,
		store.AuditPart{Name: "default/pr-review", Rows: 48000, Disk: 12 * mib})
	review.Fence2, review.Fence2Why = string(fence2.CountsOnly), "cannot read the drops the firewall logs: the kernel has no nfnetlink_log"
	airgap := vm("airgap", audit.StateVMStopped, 6*24*time.Hour, 40, 300, mib/10,
		store.AuditPart{Name: "default/corpus-scan", Rows: 12000, Disk: 4 * mib})
	scratch := vm("scratch", audit.StateVMDeleted, 12*24*time.Hour, 9, 7000, 2*mib,
		store.AuditPart{Name: "default/try", Rows: 63000, Disk: 18 * mib})
	scratch.Deleted = now.Add(-12 * 24 * time.Hour)
	a := store.Audit{Read: true, Dir: Home + "/.local/state/brig/audit", VMs: []store.AuditVM{airgap, dev, review, scratch}, Free: 812 << 30}
	for _, v := range a.VMs {
		a.Disk += v.Disk
		if a.Oldest.IsZero() || v.Oldest.Before(a.Oldest) {
			a.Oldest = v.Oldest
		}
	}
	return a
}

// History returns the demo's audit records as of now: the policy history
// of webapp on dev, the requests of spike, a sandbox deleted since, and
// what fence 2 dropped of them.
func History(now time.Time) audit.Records {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour
	rec := func(t time.Time, sandbox, kind string, fill func(*audit.Record)) audit.Record {
		r := audit.Record{V: audit.Version, T: t, VM: "dev", Workspace: store.DefaultWorkspace, Sandbox: sandbox, Kind: kind}
		fill(&r)
		return r
	}
	return audit.Records{
		rec(ago(9*day), "webapp", audit.KindSandbox, func(r *audit.Record) {
			r.Event, r.Image, r.Providers = audit.EventCreated, "localhost/pi-agent:2026-09-15", []string{"github", "llm"}
		}),
		rec(ago(9*day), "webapp", audit.KindPolicy, func(r *audit.Record) {
			r.Event, r.Version, r.Status, r.By, r.Added = audit.EventRevision, 1, "superseded", audit.BySandboxCreate, []string{"llm", "pypi"}
		}),
		rec(ago(6*day+time.Hour), "webapp", audit.KindDraft, func(r *audit.Record) {
			r.Chunk, r.Status, r.Rule, r.Endpoints, r.Binary, r.Hits = "c-7f3a", "pending", "registry_npmjs_org", []string{"registry.npmjs.org:443"}, "/usr/bin/node", 4
		}),
		rec(ago(6*day), "webapp", audit.KindDraft, func(r *audit.Record) {
			r.Chunk, r.Status, r.Rule, r.Endpoints, r.Binary, r.Decided = "c-7f3a", "approved", "registry_npmjs_org", []string{"registry.npmjs.org:443"}, "/usr/bin/node", ago(6*day)
		}),
		rec(ago(6*day), "webapp", audit.KindPolicy, func(r *audit.Record) {
			r.Event, r.Version, r.Status, r.By, r.Chunk, r.Added = audit.EventRevision, 2, "superseded", "draft c-7f3a approved", "c-7f3a", []string{"registry_npmjs_org"}
		}),
		rec(ago(3*day), "webapp", audit.KindPolicy, func(r *audit.Record) {
			r.Event, r.Version, r.Status, r.By, r.Added = audit.EventRevision, 3, "superseded", audit.ByPolicyAllow, []string{"api_github_com"}
			r.Annotations = map[string]string{"brig.dev/by": "policy-allow", "brig.dev/host": "api.github.com", "brig.dev/port": "443"}
		}),
		rec(ago(26*time.Hour), "webapp", audit.KindPolicy, func(r *audit.Record) {
			r.Event, r.Version, r.Status, r.By, r.Changed = audit.EventRevision, 4, "loaded", audit.ByUnknown, []string{"pypi"}
		}),
		rec(ago(80*time.Hour), "spike", audit.KindSandbox, func(r *audit.Record) {
			r.Event, r.Image = audit.EventCreated, "localhost/pi-agent:2026-09-15"
		}),
		rec(ago(70*time.Hour), "spike", audit.KindRequest, func(r *audit.Record) {
			r.Verdict, r.Method, r.Host, r.Port, r.Path, r.Binary, r.Rule = audit.VerdictAllow, "GET", "pypi.org", 443, "/simple/requests/", "/usr/bin/pip", "pypi"
		}),
		rec(ago(60*time.Hour), "spike", audit.KindRequest, func(r *audit.Record) {
			r.Verdict, r.Host, r.Port, r.Binary, r.Reason = audit.VerdictDeny, "paste.example.net", 443, "/usr/bin/curl", "no rule allows this endpoint"
		}),
		rec(ago(65*time.Hour), "spike", audit.KindRequest, func(r *audit.Record) {
			r.Verdict, r.Host, r.Port, r.Binary, r.Rule = audit.VerdictAllow, "nas.lan", 445, "/usr/bin/smbclient", "nas_share"
		}),
		rec(ago(65*time.Hour-50*time.Millisecond), "", audit.KindDrop, func(r *audit.Record) {
			r.Workspace, r.Reason, r.Proto, r.Host, r.Port, r.Hits, r.Last = "", fence2.ReasonLAN, "tcp", "192.168.1.20", 445, 3, ago(65*time.Hour-3*time.Second)
		}),
		rec(ago(52*time.Hour), "", audit.KindDrop, func(r *audit.Record) {
			r.Workspace, r.Reason, r.Proto, r.Host, r.Port, r.Hits = "", fence2.ReasonHostPort, "tcp", "10.0.2.2", 8080, 1
		}),
		rec(ago(50*time.Hour), "spike", audit.KindSandbox, func(r *audit.Record) { r.Event = audit.EventDeleted }),
	}
}
