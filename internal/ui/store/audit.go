// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"time"
)

// TopicAudit is the topic of the snapshot's audit trail: the collectors and
// the space their histories take.
const TopicAudit Topic = "audit"

// Audit is the state of the audit trail that brig keeps of each VM, as
// brig audit status shows it.
type Audit struct {
	// Read is false until the trail was first read.
	Read bool
	// Dir is where the histories are kept.
	Dir string
	// VMs are the VMs with a history or a collector, sorted by name.
	VMs []AuditVM
	// Disk is the space all histories take, Free what is left on their
	// file system.
	Disk, Free int64
	Oldest     time.Time
}

// AuditVM is a VM's collector and history.
type AuditVM struct {
	VM string
	// Collector says whether its collector runs, as brig audit status
	// does; Problem that nothing records the traffic of a running VM.
	Collector string
	Problem   bool
	Error     string
	Since     time.Time
	LastEvent time.Time
	Gaps24h   int
	Rows      int64
	Disk      int64
	Oldest    time.Time
	// Deleted is when brig delete deleted the VM.
	Deleted time.Time
	// Sandboxes splits the history by sandbox, WORKSPACE/NAME and empty
	// for the VM's own records, largest first; Days by day, oldest first.
	Sandboxes []AuditPart
	Days      []AuditPart
}

// AuditPart is part of a VM's history.
type AuditPart struct {
	Name string
	Rows int64
	Disk int64
	// Deleted is when a sandbox was deleted.
	Deleted time.Time
}

// Workspace is the workspace of a sandbox's part.
func (p AuditPart) Workspace() string {
	ws, _, _ := strings.Cut(p.Name, "/")
	return ws
}

// Sandbox is the name of a sandbox's part, without its workspace.
func (p AuditPart) Sandbox() string {
	_, name, _ := strings.Cut(p.Name, "/")
	return name
}

// AuditOf returns the named VM's collector and history.
func (a Audit) AuditOf(vm string) (AuditVM, bool) {
	for _, v := range a.VMs {
		if v.VM == vm {
			return v, true
		}
	}
	return AuditVM{}, false
}

// SetAudit replaces the audit trail's state.
func (s *Store) SetAudit(a Audit) {
	s.update(TopicAudit, func(sn *Snapshot) { sn.Audit = a })
}
