// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package audit keeps the audit trail of brig's VMs: what each sandbox
// reached and was denied, how its policy changed and who changed it, and
// which brig commands worked on the VM. A collector per running VM, a
// systemd user unit on the host, follows the VM's gateway and appends one
// JSON object per event to a file per day, which brig audit and brig ui
// read. The files are the record; the collector also mirrors changes to the
// user journal.
//
// The format is versioned by Record.V; later versions of brig must keep
// reading every version they wrote.
package audit

import (
	"time"

	"github.com/dennisklein/brig/internal/policy"
)

// Version is the format version of the records this brig writes.
const Version = 1

// The kinds of a Record.
const (
	// KindRequest is a request or connection that a sandbox's proxy
	// decided.
	KindRequest = "request"
	// KindGap says that records may be missing before it, and why.
	KindGap = "gap"
	// KindSandbox is a sandbox created or deleted.
	KindSandbox = "sandbox"
	// KindPolicy is a revision of a sandbox's policy, or a change of its
	// load status.
	KindPolicy = "policy"
	// KindDraft is a rule that OpenShell drafted from denied requests, or
	// a decision on one.
	KindDraft = "draft"
	// KindAction is a brig or brig ui command that worked on the VM.
	KindAction = "action"
)

// Kinds are the kinds of records, in the order brig audit lists them.
var Kinds = []string{KindRequest, KindPolicy, KindDraft, KindSandbox, KindAction, KindGap}

// The events of sandbox and policy records.
const (
	EventCreated = "created"
	EventDeleted = "deleted"
	// EventRevision is a new revision of a sandbox's policy, EventStatus a
	// change of a revision's load status.
	EventRevision = "revision"
	EventStatus   = "status"
)

// The verdicts of request records.
const (
	VerdictAllow = "allow"
	VerdictDeny  = "deny"
)

// The values of Record.By other than a draft's approval.
const (
	ByPolicyAllow   = "brig policy allow"
	BySandboxCreate = "sandbox created"
	ByUnknown       = "unknown (openshell CLI or another client)"
)

// Record is one event of a VM's audit trail: a line of a day's file. Every
// record has V, T, VM and Kind, and names its sandbox unless it is about
// the VM as a whole; the other fields belong to one kind or a few, as
// their comments say, and are left out where empty.
type Record struct {
	V         int       `json:"v"`
	T         time.Time `json:"t"`
	VM        string    `json:"vm"`
	Workspace string    `json:"workspace,omitempty"`
	Sandbox   string    `json:"sandbox,omitempty"`
	Kind      string    `json:"kind"`

	// Verdict, Method, Host, Port, Path (without its query), Binary, PID,
	// Engine, Reason and Raw (a line that could not be read, with URL
	// queries cut) describe a request. Rule is the rule that decided it,
	// or a draft's rule.
	Verdict string `json:"verdict,omitempty"`
	Method  string `json:"method,omitempty"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	Path    string `json:"path,omitempty"`
	Binary  string `json:"binary,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Rule    string `json:"rule,omitempty"`
	Engine  string `json:"engine,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Raw     string `json:"raw,omitempty"`

	// Why says why records may be missing before a gap.
	Why string `json:"why,omitempty"`

	// Event is what happened to a sandbox or policy: one of the Event
	// constants.
	Event string `json:"event,omitempty"`
	// Image, Providers and Labels describe a created sandbox.
	Image     string            `json:"image,omitempty"`
	Providers []string          `json:"providers,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`

	// Version and Hash name a policy revision. Status is its load status,
	// a draft's status (pending, approved or rejected), or an action's
	// outcome (ok, failed, canceled); Error says why it failed.
	Version uint32 `json:"version,omitempty"`
	Hash    string `json:"hash,omitempty"`
	Status  string `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
	// Added, Removed and Changed are the keys of the network rules that a
	// revision changed against the one before; Rules holds the added and
	// changed rules, and every rule of a sandbox's first revision, as the
	// policy has them.
	Added   []string               `json:"added,omitempty"`
	Removed []string               `json:"removed,omitempty"`
	Changed []string               `json:"changed,omitempty"`
	Rules   map[string]policy.Rule `json:"rules,omitempty"`
	// By says who made a revision: one of the By constants, or "draft
	// CHUNK approved". Annotations are the revision's provenance as the
	// client sent it.
	By          string            `json:"by,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// Chunk is a draft's chunk ID, or that of the draft whose approval
	// made a revision.
	Chunk string `json:"chunk,omitempty"`
	// Endpoints (as host:port) and Hits describe a draft; Decided is when
	// it was approved or rejected.
	Endpoints []string  `json:"endpoints,omitempty"`
	Hits      int       `json:"hits,omitempty"`
	Decided   time.Time `json:"decided,omitzero"`

	// Command is an action's command line, as a user would type it, and
	// Trace its trace ID in brig's activity log.
	Command string `json:"command,omitempty"`
	Trace   string `json:"trace,omitempty"`
}

// SandboxKey names the record's sandbox as WORKSPACE/NAME, or is empty for a
// record about the VM.
func (r Record) SandboxKey() string {
	if r.Sandbox == "" {
		return ""
	}
	return r.Workspace + "/" + r.Sandbox
}

// Denied reports whether the record is a denied request.
func (r Record) Denied() bool { return r.Kind == KindRequest && r.Verdict == VerdictDeny }

// Change reports whether the record tells of a change rather than of
// traffic that a rule allowed: what the journal mirror carries by default.
func (r Record) Change() bool { return r.Kind != KindRequest || r.Denied() }
