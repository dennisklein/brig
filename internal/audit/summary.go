// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"time"
)

// Reach is an endpoint that a sandbox reached or tried to, or that the VM's
// network profile dropped: what brig audit --summary lists.
type Reach struct {
	VM        string `json:"vm"`
	Workspace string `json:"workspace"`
	Sandbox   string `json:"sandbox"`
	// Endpoint is host:port.
	Endpoint string    `json:"endpoint"`
	Allowed  int       `json:"allowed"`
	Denied   int       `json:"denied"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Binaries []string  `json:"binaries"`
	// Rules are the rules that allowed it, and Via how each came into
	// the sandbox's policy: the By of the revision that last added or
	// changed it.
	Rules []string          `json:"rules,omitempty"`
	Via   map[string]string `json:"via,omitempty"`

	// Dropped counts what fence 2 dropped, for an endpoint of no sandbox,
	// which is an address and port, or empty for drops that were counted
	// only. Proto is their protocol and Reason the setting that dropped
	// them. Likely are the sandboxes they likely came from, as their
	// hints say, and FromVM counts the drops that matched none.
	Dropped int      `json:"dropped,omitempty"`
	Proto   string   `json:"proto,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Likely  []string `json:"likely,omitempty"`
	FromVM  int      `json:"from_vm,omitempty"`
}

// Summary answers what the selected sandboxes reached, and who allowed it:
// per sandbox, each endpoint of the requests q selects; then, unless q
// selects a workspace or sandbox, what each VM's network profile dropped.
func (s Store) Summary(q Query) ([]Reach, error) { return Summarize(s, q) }

// Summarize is Summary of the records that s reads.
func Summarize(s Reader, q Query) ([]Reach, error) {
	dq := q
	// How a rule came in may lie before the time range.
	origin := map[string]string{}
	pq := Query{VMs: q.VMs, Workspace: q.Workspace, Sandbox: q.Sandbox, Until: q.Until, Kinds: []string{KindPolicy}}
	err := s.Read(pq, func(r Record) bool {
		if r.Event != EventRevision {
			return true
		}
		for _, k := range append(slices.Clone(r.Added), r.Changed...) {
			origin[r.VM+"\x00"+r.SandboxKey()+"\x00"+k] = r.By
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	q.Kinds = []string{KindRequest}
	reach := map[string]*Reach{}
	err = s.Read(q, func(r Record) bool {
		if r.Host == "" {
			return true
		}
		ep := r.Host
		if r.Port != 0 {
			ep += ":" + strconv.Itoa(r.Port)
		}
		key := r.VM + "\x00" + r.SandboxKey() + "\x00" + ep
		x := reach[key]
		if x == nil {
			x = &Reach{VM: r.VM, Workspace: r.Workspace, Sandbox: r.Sandbox, Endpoint: ep, First: r.T}
			reach[key] = x
		}
		switch r.Verdict {
		case VerdictAllow:
			x.Allowed++
		case VerdictDeny:
			x.Denied++
		}
		if r.T.Before(x.First) {
			x.First = r.T
		}
		if r.T.After(x.Last) {
			x.Last = r.T
		}
		if r.Binary != "" && !slices.Contains(x.Binaries, r.Binary) {
			x.Binaries = append(x.Binaries, r.Binary)
		}
		if r.Verdict == VerdictAllow && r.Rule != "" && !slices.Contains(x.Rules, r.Rule) {
			x.Rules = append(x.Rules, r.Rule)
			if by, ok := origin[r.VM+"\x00"+r.SandboxKey()+"\x00"+r.Rule]; ok {
				if x.Via == nil {
					x.Via = map[string]string{}
				}
				x.Via[r.Rule] = by
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	out := make([]Reach, 0, len(reach))
	for _, k := range slices.Sorted(maps.Keys(reach)) {
		x := reach[k]
		slices.Sort(x.Binaries)
		slices.Sort(x.Rules)
		out = append(out, *x)
	}
	slices.SortStableFunc(out, func(a, b Reach) int {
		return cmp.Or(cmp.Compare(a.VM, b.VM), cmp.Compare(a.Workspace, b.Workspace), cmp.Compare(a.Sandbox, b.Sandbox),
			cmp.Compare(b.Denied, a.Denied), cmp.Compare(a.Endpoint, b.Endpoint))
	})
	if len(dq.Kinds) > 0 && !slices.Contains(dq.Kinds, KindDrop) {
		return out, nil
	}
	drops, err := summarizeDrops(s, dq)
	return append(out, drops...), err
}
