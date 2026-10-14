// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"reflect"
	"slices"
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/ui/store"
)

// auditEvery is how often the audit trail's state is read again.
const auditEvery = 10 * time.Second

// Audit keeps the store's audit state up to date until ctx ends, reading it
// again when the VMs change.
func Audit(ctx context.Context, st *store.Store, s audit.Store) {
	sub := st.Subscribe()
	defer sub.Close()
	tick := time.NewTicker(auditEvery)
	defer tick.Stop()
	read := func() {
		sn := st.Snapshot()
		if a := ReadAudit(ctx, sn, s); !reflect.DeepEqual(a, sn.Audit) {
			st.SetAudit(a)
		}
	}
	read()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Changed():
			// Only a change of the VMs can change what is read. Reading
			// for the audit's own change would loop.
			if !slices.Contains(sub.Take(), store.TopicVMs) {
				continue
			}
			read()
		case <-tick.C:
			read()
		}
	}
}

// ReadAudit reads the state of the audit trail of the snapshot's VMs and
// of those that have a history.
func ReadAudit(ctx context.Context, sn store.Snapshot, s audit.Store) store.Audit {
	out := store.Audit{Read: true, Dir: s.Root}
	vms, _ := s.VMs()
	for _, v := range sn.VMs {
		if !slices.Contains(vms, v.Name) {
			vms = append(vms, v.Name)
		}
	}
	slices.Sort(vms)
	statuses, err := s.Status(ctx, vms, func(name string) audit.VMInfo {
		v, ok := sn.VM(name)
		return audit.VMInfo{Exists: ok, Running: v.Running(), Known: ok && v.State != ""}
	})
	if err != nil {
		return out
	}
	for _, st := range statuses {
		a := store.AuditVM{
			VM: st.VM, Collector: st.Collector, Problem: st.Problem(), Error: st.Error, Since: st.Since, LastEvent: st.LastEvent,
			Fence2: string(st.Fence2), Fence2Why: st.Fence2Why,
			Gaps24h: st.Gaps24h, Rows: st.Rows, Disk: st.Disk, Oldest: st.Oldest, Deleted: st.Deleted,
		}
		if x, err := s.ReadIndex(st.VM); err == nil {
			for _, p := range x.Breakdown().Sandboxes {
				a.Sandboxes = append(a.Sandboxes, store.AuditPart{Name: p.Name, Rows: p.Rows, Disk: p.Disk, Deleted: p.Deleted})
			}
			for _, day := range x.DayNames() {
				d := x.Days[day]
				a.Days = append(a.Days, store.AuditPart{Name: day, Rows: d.Rows(), Disk: d.Disk})
			}
		}
		out.VMs = append(out.VMs, a)
		out.Disk += st.Disk
		if !st.Oldest.IsZero() && (out.Oldest.IsZero() || st.Oldest.Before(out.Oldest)) {
			out.Oldest = st.Oldest
		}
	}
	out.Free, _ = s.Free()
	return out
}
