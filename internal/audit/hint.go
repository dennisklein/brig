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

// HintBefore is how long before a drop the request that fence 1 allowed
// for it may lie.
const HintBefore = 2 * time.Second

// Hint says which sandbox a drop likely came from. Fence 2 sees packets
// from the VM, not from sandboxes, so this is a guess from the requests
// that fence 1 allowed to the same port just before: worked out when the
// trail is read, never stored.
type Hint struct {
	// Sandboxes are the sandboxes, as WORKSPACE/NAME, whose allowed
	// requests match, sorted; none says the drop came from the VM itself,
	// not through a sandbox's proxy.
	Sandboxes []string `json:"sandboxes,omitempty"`
	// Host, Port and At are the latest matching request's host, port and
	// time.
	Host string    `json:"host,omitempty"`
	Port int       `json:"port,omitempty"`
	At   time.Time `json:"at,omitzero"`
}

// Hinter matches drops with the requests that fence 1 allowed. It keeps the
// allowed requests it observes, which must come in time order to be
// forgotten in it.
type Hinter struct {
	// reqs are the allowed requests by VM and port, oldest first.
	reqs map[hintKey][]Record
}

type hintKey struct {
	vm   string
	port int
}

// Observe keeps r if it is an allowed request that a drop may match.
func (h *Hinter) Observe(r Record) {
	if r.Kind != KindRequest || r.Verdict != VerdictAllow || r.Port == 0 || r.Sandbox == "" {
		return
	}
	if h.reqs == nil {
		h.reqs = map[hintKey][]Record{}
	}
	k := hintKey{r.VM, r.Port}
	h.reqs[k] = append(h.reqs[k], r)
}

// Forget drops the requests before t.
func (h *Hinter) Forget(t time.Time) {
	for k, rs := range h.reqs {
		i, _ := slices.BinarySearchFunc(rs, t, func(r Record, t time.Time) int { return r.T.Compare(t) })
		if i == len(rs) {
			delete(h.reqs, k)
		} else if i > 0 {
			h.reqs[k] = slices.Clone(rs[i:])
		}
	}
}

// Hint matches the drop d with the allowed requests to its port of its VM
// from HintBefore before its first packet up to its last. A drop that was
// counted only has no address and port, and so no hint.
func (h *Hinter) Hint(d Record) (Hint, bool) {
	if d.Kind != KindDrop || d.Port == 0 {
		return Hint{}, false
	}
	rs := h.reqs[hintKey{d.VM, d.Port}]
	from, until := d.T.Add(-HintBefore), d.T
	if d.Last.After(until) {
		until = d.Last
	}
	i, _ := slices.BinarySearchFunc(rs, from, func(r Record, t time.Time) int { return r.T.Compare(t) })
	var out Hint
	seen := map[string]bool{}
	for ; i < len(rs) && !rs[i].T.After(until); i++ {
		r := rs[i]
		if k := r.SandboxKey(); !seen[k] {
			seen[k] = true
			out.Sandboxes = append(out.Sandboxes, k)
		}
		out.Host, out.Port, out.At = r.Host, r.Port, r.T
	}
	slices.Sort(out.Sandboxes)
	return out, true
}

// DropKey identifies a drop record among those Hints returns.
func DropKey(r Record) string {
	return r.VM + "\x00" + strconv.FormatInt(r.T.UnixNano(), 36) + "\x00" + r.Reason + "\x00" + r.Proto + "\x00" + r.Host + "\x00" + strconv.Itoa(r.Port)
}

// Hints works out the hints of the drops of the VMs and time range that q
// selects, by DropKey.
func Hints(s Reader, q Query) (map[string]Hint, error) {
	dq := Query{VMs: q.VMs, Since: q.Since, Until: q.Until, Kinds: []string{KindDrop}, Host: q.Host}
	var drops []Record
	ports := map[hintKey]bool{}
	err := s.Read(dq, func(r Record) bool {
		if r.Port != 0 {
			drops = append(drops, r)
			ports[hintKey{r.VM, r.Port}] = true
		}
		return true
	})
	if err != nil || len(drops) == 0 {
		return nil, err
	}
	rq := Query{VMs: q.VMs, Until: q.Until, Kinds: []string{KindRequest}}
	if !q.Since.IsZero() {
		rq.Since = q.Since.Add(-HintBefore)
	}
	h := &Hinter{}
	err = s.Read(rq, func(r Record) bool {
		if ports[hintKey{r.VM, r.Port}] {
			h.Observe(r)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]Hint, len(drops))
	for _, d := range drops {
		if hint, ok := h.Hint(d); ok {
			out[DropKey(d)] = hint
		}
	}
	return out, nil
}

// summarizeDrops sums up the drops that q selects per VM, reason,
// protocol, address and port, with the sandboxes they likely came from,
// the most frequent first per VM.
func summarizeDrops(s Reader, q Query) ([]Reach, error) {
	if q.Workspace != "" || q.Sandbox != "" {
		// Drops belong to no sandbox.
		return nil, nil
	}
	hints, err := Hints(s, q)
	if err != nil {
		return nil, err
	}
	q.Kinds, q.Denied = []string{KindDrop}, false
	byKey := map[string]*Reach{}
	err = s.Read(q, func(r Record) bool {
		k := r.VM + "\x00" + r.Reason + "\x00" + r.Proto + "\x00" + DropTarget(r)
		x := byKey[k]
		if x == nil {
			x = &Reach{VM: r.VM, Endpoint: DropTarget(r), Proto: r.Proto, Reason: r.Reason, First: r.T}
			byKey[k] = x
		}
		x.Dropped += r.Hits
		x.First = minTime(x.First, r.T)
		x.Last = maxTime(x.Last, cmp.Or(r.Last, r.T))
		if hint, ok := hints[DropKey(r)]; ok {
			if len(hint.Sandboxes) == 0 {
				x.FromVM++
			}
			for _, sb := range hint.Sandboxes {
				if !slices.Contains(x.Likely, sb) {
					x.Likely = append(x.Likely, sb)
				}
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	out := make([]Reach, 0, len(byKey))
	for _, k := range slices.Sorted(maps.Keys(byKey)) {
		x := byKey[k]
		slices.Sort(x.Likely)
		out = append(out, *x)
	}
	slices.SortStableFunc(out, func(a, b Reach) int {
		return cmp.Or(cmp.Compare(a.VM, b.VM), cmp.Compare(b.Dropped, a.Dropped))
	})
	return out, nil
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
