// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"cmp"
	"context"
	"maps"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// The states of a collector as Status reports them, besides the unit's.
const (
	// StateNotRunning: the VM runs, its collector does not.
	StateNotRunning = "not running"
	// StateVMStopped: the VM and its collector are stopped.
	StateVMStopped = "stopped (VM stopped)"
	// StateVMDeleted: brig delete deleted the VM; its history stays.
	StateVMDeleted = "VM deleted"
)

// VMStatus is the state of a VM's collector and the size of its history.
type VMStatus struct {
	VM string `json:"vm"`
	// Collector is UnitRunning, UnitFailed, or one of the State constants.
	Collector string `json:"collector"`
	// Error is the failed collector's last line, or why a running one
	// cannot read the gateway.
	Error     string    `json:"error,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	Heartbeat time.Time `json:"heartbeat,omitzero"`
	LastEvent time.Time `json:"last_event,omitzero"`
	Gaps24h   int       `json:"gaps_24h"`
	Rows      int64     `json:"rows"`
	Disk      int64     `json:"disk"`
	Oldest    time.Time `json:"oldest,omitzero"`
	Deleted   time.Time `json:"deleted,omitzero"`
}

// Problem reports whether the VM's history has a hole now: its collector
// failed or does not run while the VM does.
func (v VMStatus) Problem() bool {
	return v.Collector == UnitFailed || v.Collector == StateNotRunning
}

// VMInfo is what Status needs to know of a VM that brig has.
type VMInfo struct {
	// Exists is false for a VM that brig no longer has.
	Exists bool
	// Running is whether it runs; Known whether that could be found out.
	Running, Known bool
}

// Status reports on the collectors and histories of the VMs; info says
// what brig knows of each. ctx bounds asking systemd.
func (s Store) Status(ctx context.Context, vms []string, info func(vm string) VMInfo) ([]VMStatus, error) {
	now := time.Now()
	out := make([]VMStatus, 0, len(vms))
	for _, vm := range vms {
		x, err := s.ReadIndex(vm)
		if err != nil {
			return nil, err
		}
		st := VMStatus{
			VM: vm, Heartbeat: x.Collector.Heartbeat, LastEvent: x.Collector.LastEvent,
			Gaps24h: x.GapsSince(now.Add(-24 * time.Hour)), Disk: x.Disk(), Deleted: x.Deleted,
		}
		for _, d := range x.Days {
			st.Rows += d.Rows()
			if !d.First.IsZero() && (st.Oldest.IsZero() || d.First.Before(st.Oldest)) {
				st.Oldest = d.First
			}
		}
		in := info(vm)
		collecting := s.Collecting(vm)
		switch {
		case !in.Exists:
			st.Collector = StateVMDeleted
			if st.Deleted.IsZero() {
				st.Deleted = x.Collector.Stopped
			}
		case collecting:
			st.Collector, st.Since, st.Error = UnitRunning, x.Collector.Started, x.Collector.Err
		case UnitState(ctx, vm) == UnitFailed:
			st.Collector, st.Error = UnitFailed, UnitError(ctx, vm)
		case in.Known && !in.Running:
			st.Collector = StateVMStopped
		case in.Known:
			st.Collector = StateNotRunning
		default:
			st.Collector = UnitStopped
		}
		out = append(out, st)
	}
	return out, nil
}

// Free is the space left on the file system of the store.
func (s Store) Free() (int64, error) {
	dir := s.Root
	var fs syscall.Statfs_t
	for {
		err := syscall.Statfs(dir, &fs)
		if err == nil {
			return int64(fs.Bavail) * fs.Bsize, nil //nolint:gosec // a file system's size fits.
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, err
		}
		dir = parent
	}
}

// Part is a part of a VM's history in a breakdown, with its rows and size
// on disk, estimated from the day files' ratio of disk to bytes written.
type Part struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
	Disk int64  `json:"disk"`
	// Deleted is when a sandbox was deleted.
	Deleted time.Time `json:"deleted,omitzero"`
}

// Breakdown splits a VM's history by sandbox, kind and month.
type Breakdown struct {
	Sandboxes []Part `json:"sandboxes"`
	Kinds     []Part `json:"kinds"`
	Months    []Part `json:"months"`
}

// Breakdown sums the index up by sandbox (the VM's own records under ""),
// kind and month.
func (x *Index) Breakdown() Breakdown {
	sandboxes, kinds, months := map[string]*Part{}, map[string]*Part{}, map[string]*Part{}
	add := func(m map[string]*Part, name string, rows, disk int64) {
		p := m[name]
		if p == nil {
			p = &Part{Name: name}
			m[name] = p
		}
		p.Rows += rows
		p.Disk += disk
	}
	for name, d := range x.Days {
		month := name[:min(len(name), 7)]
		for sb, ks := range d.Counts {
			for kind, c := range ks {
				disk := c.Bytes
				if d.Bytes > 0 {
					disk = c.Bytes * d.Disk / d.Bytes
				}
				add(sandboxes, sb, c.Rows, disk)
				add(kinds, kind, c.Rows, disk)
				add(months, month, c.Rows, disk)
			}
		}
	}
	for name, p := range sandboxes {
		p.Deleted = x.Sandboxes[name]
	}
	list := func(m map[string]*Part, order func(a, b *Part) int) []Part {
		ps := slices.SortedFunc(maps.Values(m), order)
		out := make([]Part, len(ps))
		for i, p := range ps {
			out[i] = *p
		}
		return out
	}
	bySize := func(a, b *Part) int {
		return cmp.Or(cmp.Compare(b.Disk, a.Disk), cmp.Compare(a.Name, b.Name))
	}
	return Breakdown{
		Sandboxes: list(sandboxes, bySize),
		Kinds:     list(kinds, bySize),
		Months:    list(months, func(a, b *Part) int { return cmp.Compare(a.Name, b.Name) }),
	}
}
