// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TopicTraffic is the topic of the snapshot's traffic: the requests that
// sandboxes made and the rules OpenShell drafted from the denied ones.
const TopicTraffic Topic = "traffic"

// MaxRequests is how many requests the store keeps per gateway; older ones
// go first.
const MaxRequests = 10000

// The verdicts of a Request.
const (
	VerdictAllow = "allow"
	VerdictDeny  = "deny"
)

// Request is a line of a sandbox's log that tells of a network request or
// connection, as OpenShell's proxy decided it (fence 1). The gateway sends
// it as display text; what brig ui could read from it is in the fields, and
// a line it could not read is kept in Raw.
type Request struct {
	// Seq numbers the requests of the store, from 1, in the order they
	// arrived.
	Seq     uint64
	VM      string
	Sandbox string
	Time    time.Time
	// Verdict is VerdictAllow or VerdictDeny, empty when unknown.
	Verdict string
	// Method is the HTTP method, empty for a connection.
	Method string
	// Process is the program as the proxy named it: a path such as
	// /usr/bin/curl, or only a name such as curl. Empty when the line names
	// none.
	Process string
	PID     int
	Host    string
	Port    int
	// Path is the HTTP request's path, without its query, which may hold
	// a secret.
	Path string
	// Policy and Engine name the rule that decided, and how.
	Policy, Engine string
	// Reason says why it was denied.
	Reason string
	// Raw is the line as it came, with URL queries cut, when brig ui could
	// not read it.
	Raw string
	// Gap says that requests may be missing before this one, and why. A
	// gap row has no request.
	Gap string
}

// Denied reports whether the proxy denied the request.
func (r Request) Denied() bool { return r.Verdict == VerdictDeny }

// Endpoint is the host and port of the request, as host:port.
func (r Request) Endpoint() string {
	if r.Port == 0 {
		return r.Host
	}
	return r.Host + ":" + strconv.Itoa(r.Port)
}

// CredentialMismatch reports whether the request was denied because a
// provider's credential may not go to its host.
func (r Request) CredentialMismatch() bool {
	return strings.Contains(r.Reason, "credential_endpoint_mismatch")
}

// Draft is a network rule that OpenShell drafted from a sandbox's denied
// requests and that waits for approval.
type Draft struct {
	Sandbox string
	// ID is the draft's chunk ID, which openshell rule approve takes.
	ID string
	// Rule is the rule's key under network_policies.
	Rule string
	// Binary is the full path of the program whose requests were denied.
	Binary string
	// Endpoints are the rule's endpoints.
	Endpoints []DraftEndpoint
	// Binaries are the programs the rule allows.
	Binaries []string
	// YAML is the rule as a policy file writes it, under its key.
	YAML string
	// Rationale says why OpenShell proposes it; Validation is the
	// prover's verdict, and Security the concerns it raised, if any.
	Rationale, Validation, Security string
	// Error says why the gateway cannot apply it now, if it cannot.
	Error string
	// Hits counts how often the denied endpoint was seen.
	Hits     int
	LastSeen time.Time
}

// DraftEndpoint is a host and port that a drafted rule allows.
type DraftEndpoint struct {
	Host string
	Port int
}

// Matches reports whether the draft would allow the request: it names the
// request's host and port, and the request's program, if the line named
// one, by its path or its name.
func (d Draft) Matches(r Request) bool {
	if r.Sandbox != d.Sandbox || r.Host == "" {
		return false
	}
	if !slices.ContainsFunc(d.Endpoints, func(e DraftEndpoint) bool {
		return strings.EqualFold(e.Host, r.Host) && (e.Port == r.Port || r.Port == 0)
	}) {
		return false
	}
	if r.Process == "" || r.Process == "-" || d.Binary == "" {
		return true
	}
	return r.Process == d.Binary || r.Process == path.Base(d.Binary)
}

// Traffic is what brig ui read of a gateway's traffic.
type Traffic struct {
	VM string
	// Requests are oldest first. The store appends to them in place: a
	// snapshot's slice never changes, but it may share its array with a
	// later one.
	Requests []Request
	// Drafts are the pending drafted rules, by sandbox and rule.
	Drafts []Draft
}

// Draft returns the pending drafted rule of the sandbox with the chunk ID
// id.
func (t Traffic) Draft(sandbox, id string) (Draft, bool) {
	for _, d := range t.Drafts {
		if d.Sandbox == sandbox && d.ID == id {
			return d, true
		}
	}
	return Draft{}, false
}

// DraftFor returns the pending drafted rule that would allow the request,
// if there is exactly one.
func (t Traffic) DraftFor(r Request) (Draft, bool) {
	var found []Draft
	for _, d := range t.Drafts {
		if d.Matches(r) {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		return Draft{}, false
	}
	return found[0], true
}

// Request returns the request numbered seq.
func (t Traffic) Request(seq uint64) (Request, bool) {
	i, ok := slices.BinarySearchFunc(t.Requests, seq, func(r Request, seq uint64) int {
		switch {
		case r.Seq < seq:
			return -1
		case r.Seq > seq:
			return 1
		}
		return 0
	})
	if !ok {
		return Request{}, false
	}
	return t.Requests[i], true
}

// trafficIndex returns the index of the VM's traffic in ts, adding it in
// order if it is not there. ts is changed in place.
func trafficIndex(ts *[]Traffic, vm string) int {
	i, found := slices.BinarySearchFunc(*ts, vm, func(t Traffic, name string) int { return strings.Compare(t.VM, name) })
	if !found {
		*ts = slices.Insert(*ts, i, Traffic{VM: vm})
	}
	return i
}

// AddRequests adds requests of the gateway of the VM named vm, numbering
// them. The oldest go when there are more than MaxRequests.
func (s *Store) AddRequests(vm string, rs []Request) {
	if len(rs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := slices.Clone(s.snap.Traffic)
	i := trafficIndex(&ts, vm)
	t := &ts[i]
	reqs := t.Requests
	if len(reqs)+len(rs) > MaxRequests {
		// A new array, so that earlier snapshots keep theirs; twice the
		// limit, so that this happens once per MaxRequests requests.
		keep := max(0, MaxRequests-len(rs))
		reqs = append(make([]Request, 0, 2*MaxRequests), reqs[len(reqs)-min(keep, len(reqs)):]...)
		rs = rs[max(0, len(rs)-MaxRequests):]
	}
	for _, r := range rs {
		s.seq++
		r.Seq, r.VM = s.seq, vm
		// Earlier snapshots end before this index, so appending in place
		// leaves what they hold alone.
		reqs = append(reqs, r)
	}
	t.Requests = reqs
	s.snap.Traffic = ts
	s.notify(TopicTraffic)
}

// SetDrafts replaces the pending drafted rules of a sandbox of the VM's
// gateway. When they did not change, nothing is published.
func (s *Store) SetDrafts(vm, sandbox string, drafts []Draft) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := slices.Clone(s.snap.Traffic)
	i := trafficIndex(&ts, vm)
	t := &ts[i]
	var old []Draft
	kept := make([]Draft, 0, len(t.Drafts)+len(drafts))
	for _, d := range t.Drafts {
		if d.Sandbox == sandbox {
			old = append(old, d)
		} else {
			kept = append(kept, d)
		}
	}
	if draftsEqual(old, drafts) {
		return
	}
	for _, d := range drafts {
		d.Sandbox = sandbox
		kept = append(kept, d)
	}
	slices.SortFunc(kept, func(a, b Draft) int {
		if c := strings.Compare(a.Sandbox, b.Sandbox); c != 0 {
			return c
		}
		if c := strings.Compare(a.Rule, b.Rule); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	t.Drafts = kept
	s.snap.Traffic = ts
	s.notify(TopicTraffic)
}

func draftsEqual(a, b []Draft) bool {
	return slices.EqualFunc(a, b, func(x, y Draft) bool {
		y.Sandbox = x.Sandbox
		return x.ID == y.ID && x.Rule == y.Rule && x.Binary == y.Binary && x.YAML == y.YAML &&
			x.Rationale == y.Rationale && x.Validation == y.Validation && x.Security == y.Security &&
			x.Error == y.Error && x.Hits == y.Hits && x.LastSeen.Equal(y.LastSeen) &&
			slices.Equal(x.Endpoints, y.Endpoints) && slices.Equal(x.Binaries, y.Binaries)
	})
}

// RemoveTraffic drops the traffic of the VMs that keep does not hold.
func (s *Store) RemoveTraffic(keep func(vm string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := slices.DeleteFunc(slices.Clone(s.snap.Traffic), func(t Traffic) bool { return !keep(t.VM) })
	if len(ts) == len(s.snap.Traffic) {
		return
	}
	s.snap.Traffic = ts
	s.notify(TopicTraffic)
}

// TrafficOf returns the traffic of the VM named vm.
func (sn Snapshot) TrafficOf(vm string) Traffic {
	for _, t := range sn.Traffic {
		if t.VM == vm {
			return t
		}
	}
	return Traffic{VM: vm}
}
