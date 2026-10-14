// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/activity"
	"github.com/dennisklein/brig/internal/fence2"
	"github.com/dennisklein/brig/internal/ocwatch"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/policy"
	"github.com/dennisklein/brig/internal/ui/store"
)

// Gateway is what the collector reads from a VM's gateway; osclient.Client
// is one.
type Gateway interface {
	ocwatch.Client
	Version(ctx context.Context) (string, error)
	Sandboxes(ctx context.Context) ([]store.Sandbox, error)
	Revisions(ctx context.Context, workspace, sandbox string, after uint32) ([]osclient.Revision, error)
	AllDrafts(ctx context.Context, workspace, sandbox string) ([]store.Draft, error)
	Close() error
}

// Collector follows one VM's gateway, brig's activity log and the spool of
// what the VM's network profile drops, and writes what it reads to the VM's
// audit trail. One runs per running VM, as the
// systemd user unit that UnitName names.
type Collector struct {
	VM    string
	Store Store
	// Dial connects to the VM's gateway.
	Dial func() (Gateway, error)
	// ActivityLog is brig's activity log, empty for none.
	ActivityLog string
	// Journal mirrors the records, nil for none.
	Journal *Journal
	// Retention deletes the days older than it, zero for none.
	Retention time.Duration
	// Log tells what the collector cannot do; the unit's journal keeps it.
	Log *slog.Logger

	// The intervals; zero picks the defaults.
	SandboxesEvery, PoliciesEvery, FlushEvery, Backoff time.Duration
}

// Default intervals of a Collector.
const (
	sandboxesEvery = 5 * time.Second
	policiesEvery  = 30 * time.Second
	flushEvery     = time.Second
	// draftsAfter is how long after a denial the sandbox's drafts are read:
	// OpenShell drafts a rule from denials in batches.
	draftsAfter = 3 * time.Second
	// draftMatch is how far apart the decision on a draft and the
	// revision it made may lie.
	draftMatch = 10 * time.Second
	// actionsKept is how many recorded runs the positions remember.
	actionsKept = 200
)

// positions is where a collector resumes: what it has recorded already.
type positions struct {
	// Logs are the places in the sandboxes' logs.
	Logs map[string]ocwatch.Position `json:"logs"`
	// Sandboxes are the sandboxes the collector knows, by WORKSPACE/NAME.
	Sandboxes map[string]*sandboxState `json:"sandboxes"`
	// Actions are the IDs of the newest runs recorded, Until the end of
	// the newest; older runs are recorded already.
	Actions []string  `json:"actions,omitempty"`
	Until   time.Time `json:"actions_until,omitzero"`
	// Clean says that the collector ended after its gateway had gone, as
	// when the VM stopped: the gateway's logs start afresh, and no
	// record is missing.
	Clean bool `json:"clean,omitempty"`
	// Retained is the date the retention was last applied.
	Retained string `json:"retained,omitempty"`
	// Spool is the place in the spool of fence 2's drops.
	Spool fence2.Position `json:"spool,omitzero"`
}

type sandboxState struct {
	Created time.Time `json:"created,omitzero"`
	// Policy is the newest revision recorded; Pending are those recorded
	// while pending, whose outcome is still to record.
	Policy  uint32   `json:"policy,omitempty"`
	Pending []uint32 `json:"pending,omitempty"`
	// Loaded is the version the sandbox loaded when last listed.
	Loaded uint32 `json:"loaded,omitempty"`
	// Drafts are the drafts' statuses by chunk ID.
	Drafts map[string]string `json:"drafts,omitempty"`
}

// collectorRun is the state of a running collector.
type collectorRun struct {
	*Collector
	w   *Writer
	pos positions
	// docs are the policies of the newest revisions recorded, by sandbox.
	docs    map[string]*policy.Doc
	watcher *ocwatch.Watcher
	// soon are the sandboxes whose policies and drafts to read at the
	// time given.
	soon chan string
	due  map[string]time.Time
	tail *activity.Tail
	red  *activity.Reducer
}

// Run collects until ctx ends.
func (c *Collector) Run(ctx context.Context) error {
	w, err := c.Store.OpenWriter(c.VM)
	if err != nil {
		return err
	}
	r := &collectorRun{
		Collector: c, w: w, docs: map[string]*policy.Doc{},
		soon: make(chan string, 64), due: map[string]time.Time{},
	}
	if err := c.Store.ReadPositions(c.VM, &r.pos); err != nil {
		c.log().Warn("cannot read where the audit trail ended; starting afresh", "err", err)
		r.pos = positions{}
	}
	if r.pos.Sandboxes == nil {
		r.pos.Sandboxes = map[string]*sandboxState{}
	}
	logs := map[ocwatch.Key]ocwatch.Position{}
	for k, p := range r.pos.Logs {
		ws, name, _ := strings.Cut(k, "/")
		if r.pos.Clean {
			// The gateway has restarted since: its cursors are new.
			p.Cursor = ""
		}
		logs[ocwatch.Key{Workspace: ws, Name: name}] = p
	}
	now := time.Now()
	if hb := w.index.Collector.Heartbeat; !r.pos.Clean && !hb.IsZero() && now.Sub(hb) > ocwatch.Backfill {
		// The gateway keeps the lines of the last hour or so only.
		r.add(Record{T: now, Kind: KindGap, Why: "The audit collector did not run since " + hb.Local().Format(time.DateTime) + " while the VM may have; requests from then may be missing."})
	}
	r.pos.Clean = false
	if r.pos.Until.IsZero() {
		r.pos.Until = time.Now().Add(-ocwatch.Backfill)
	}
	r.watcher = &ocwatch.Watcher{
		Who:     "The audit collector",
		Request: r.request,
		Gap: func(k ocwatch.Key, at time.Time, why string) {
			r.add(Record{T: at, Workspace: k.Workspace, Sandbox: k.Name, Kind: KindGap, Why: why})
		},
		Log: c.log(),
	}
	r.watcher.SetPositions(logs)
	if c.ActivityLog != "" {
		r.red = activity.NewReducer(actionsKept)
		r.tail = activity.NewTail(c.ActivityLog, r.red)
	}
	w.Update(func(s *CollectorState) {
		*s = CollectorState{Started: now, Gaps: s.Gaps, LastEvent: s.LastEvent, Fence2: s.Fence2, Fence2Why: s.Fence2Why}
	})
	r.retain(now)
	gone := r.loop(ctx)
	r.pos.Clean = gone
	return w.Close(r.positions())
}

func (c *Collector) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

// conn is a connection to the gateway, with the context of its watches.
type conn struct {
	g      Gateway
	ctx    context.Context
	cancel context.CancelFunc
}

// loop reads the gateway until ctx ends, and reports whether the gateway
// had gone by then.
func (r *collectorRun) loop(ctx context.Context) bool {
	tick := time.NewTicker(cmp.Or(r.FlushEvery, flushEvery))
	defer tick.Stop()
	first := cmp.Or(r.Backoff, time.Second)
	backoff := first
	var cn *conn
	disconnect := func() {
		if cn == nil {
			return
		}
		cn.cancel()
		r.watcher.Wait()
		_ = cn.g.Close()
		cn = nil
	}
	defer disconnect()
	var nextList, nextPolicies time.Time
	retry := func(err error) {
		r.failed(err)
		disconnect()
		nextList = time.Now().Add(backoff)
		backoff = min(2*backoff, 30*time.Second)
	}
	for {
		now := time.Now()
		if cn == nil && !now.Before(nextList) {
			if g, err := r.Dial(); err != nil {
				retry(err)
			} else {
				cctx, cancel := context.WithCancel(ctx)
				cn = &conn{g: g, ctx: cctx, cancel: cancel}
			}
		}
		if cn != nil && !now.Before(nextList) {
			if err := r.sandboxes(cn); err != nil {
				if ctx.Err() != nil {
					break
				}
				retry(err)
			} else {
				r.w.Update(func(s *CollectorState) { s.Err = "" })
				backoff = first
				nextList = now.Add(cmp.Or(r.SandboxesEvery, sandboxesEvery))
			}
		}
		if cn != nil && !now.Before(nextPolicies) {
			for k := range r.pos.Sandboxes {
				r.due[k] = now
			}
			nextPolicies = now.Add(cmp.Or(r.PoliciesEvery, policiesEvery))
		}
		if cn != nil {
			r.policies(cn)
		}
		select {
		case <-ctx.Done():
		case k := <-r.soon:
			if _, ok := r.due[k]; !ok {
				r.due[k] = time.Now().Add(draftsAfter)
			}
			continue
		case <-tick.C:
			r.actions()
			r.drops()
			r.retain(time.Now())
			if err := r.w.Flush(r.positions()); err != nil {
				r.log().Error("cannot write the audit trail", "err", err)
			}
			continue
		}
		break
	}
	// Whether the gateway still answers tells a VM that stopped from a
	// collector stopped by hand.
	if cn == nil {
		return true
	}
	vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, err := cn.g.Version(vctx)
	return err != nil
}

// failed notes why the gateway cannot be read.
func (r *collectorRun) failed(err error) {
	msg := err.Error()
	logged := false
	r.w.Update(func(s *CollectorState) {
		logged = s.Err == msg
		s.Err = msg
	})
	if !logged {
		r.log().Warn("cannot read the gateway", "vm", r.VM, "err", msg)
	}
}

// add writes records and mirrors them.
func (r *collectorRun) add(recs ...Record) {
	r.w.Add(recs...)
	for _, rec := range recs {
		rec.V, rec.VM = Version, r.VM
		if err := r.Journal.Send(rec); err != nil {
			r.log().Debug("cannot mirror to the journal", "err", err)
		}
	}
}

// request records a request that a watch read.
func (r *collectorRun) request(k ocwatch.Key, q store.Request) {
	r.add(Record{
		T: q.Time, Workspace: k.Workspace, Sandbox: k.Name, Kind: KindRequest,
		Verdict: q.Verdict, Method: q.Method, Host: q.Host, Port: q.Port, Path: q.Path,
		Binary: q.Process, PID: q.PID, Rule: q.Policy, Engine: q.Engine, Reason: q.Reason, Raw: q.Raw,
	})
	if q.Denied() {
		// OpenShell may draft a rule from it.
		select {
		case r.soon <- k.String():
		default:
		}
	}
}

// sandboxes lists the sandboxes, records those created and deleted, and
// follows their logs.
func (r *collectorRun) sandboxes(cn *conn) error {
	list, err := cn.g.Sandboxes(cn.ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	listed := map[string]bool{}
	var keys []ocwatch.Key
	for _, s := range list {
		k := ocwatch.Key{Workspace: s.Workspace, Name: s.Name}
		key := k.String()
		listed[key] = true
		keys = append(keys, k)
		st := r.pos.Sandboxes[key]
		if st != nil && !s.Created.IsZero() && !st.Created.IsZero() && !s.Created.Equal(st.Created) {
			// Deleted and created again between two lists.
			r.deleted(key, now)
			st = nil
		}
		if st == nil {
			st = &sandboxState{Created: s.Created, Drafts: map[string]string{}}
			r.pos.Sandboxes[key] = st
			r.add(Record{
				T: cmp.Or(s.Created, now), Workspace: s.Workspace, Sandbox: s.Name, Kind: KindSandbox, Event: EventCreated,
				Image: s.Image, Providers: s.Providers, Labels: s.Labels,
			})
			r.due[key] = now
		}
		if s.PolicyVersion != st.Loaded {
			st.Loaded = s.PolicyVersion
			r.due[key] = now
		}
	}
	for key := range r.pos.Sandboxes {
		if !listed[key] {
			r.deleted(key, now)
		}
	}
	r.watcher.Follow(cn.ctx, cn.g, keys)
	return nil
}

func (r *collectorRun) deleted(key string, at time.Time) {
	ws, name, _ := strings.Cut(key, "/")
	r.add(Record{T: at, Workspace: ws, Sandbox: name, Kind: KindSandbox, Event: EventDeleted})
	delete(r.pos.Sandboxes, key)
	delete(r.docs, key)
	delete(r.due, key)
}

// policies reads the policies and drafts of the sandboxes that are due.
func (r *collectorRun) policies(cn *conn) {
	now := time.Now()
	for _, key := range slices.Sorted(maps.Keys(r.due)) {
		if r.due[key].After(now) {
			continue
		}
		delete(r.due, key)
		st := r.pos.Sandboxes[key]
		if st == nil {
			continue
		}
		if err := r.policy(cn, key, st); err != nil && cn.ctx.Err() == nil {
			r.log().Warn("cannot read a sandbox's policy", "sandbox", key, "err", err)
		}
	}
}

// policy records the sandbox's new drafts and decisions, its new policy
// revisions and the outcomes of those that were pending.
func (r *collectorRun) policy(cn *conn, key string, st *sandboxState) error {
	ws, name, _ := strings.Cut(key, "/")
	drafts, err := cn.g.AllDrafts(cn.ctx, ws, name)
	if err != nil {
		return err
	}
	for _, d := range drafts {
		if st.Drafts[d.ID] == d.Status {
			continue
		}
		if st.Drafts == nil {
			st.Drafts = map[string]string{}
		}
		st.Drafts[d.ID] = d.Status
		rec := Record{
			T: time.Now(), Workspace: ws, Sandbox: name, Kind: KindDraft,
			Chunk: d.ID, Status: d.Status, Rule: d.Rule, Binary: d.Binary, Hits: d.Hits, Decided: d.Decided,
		}
		if d.Status != "pending" && !d.Decided.IsZero() {
			rec.T = d.Decided
		}
		for _, ep := range d.Endpoints {
			rec.Endpoints = append(rec.Endpoints, ep.Host+":"+strconv.Itoa(ep.Port))
		}
		r.add(rec)
	}
	after := st.Policy
	if r.docs[key] == nil && after > 0 {
		// The newest revision recorded is the base of the next one's diff.
		after--
	}
	for _, v := range st.Pending {
		after = min(after, v-1)
	}
	revs, err := cn.g.Revisions(cn.ctx, ws, name, after)
	if err != nil {
		return err
	}
	for _, rev := range revs {
		if rev.Version <= st.Policy {
			if rev.Version == st.Policy && r.docs[key] == nil {
				r.docs[key], _ = parseRevision(rev)
			}
			if slices.Contains(st.Pending, rev.Version) && rev.Status != "pending" {
				st.Pending = slices.DeleteFunc(st.Pending, func(v uint32) bool { return v == rev.Version })
				r.add(Record{
					T: cmp.Or(rev.Loaded, time.Now()), Workspace: ws, Sandbox: name, Kind: KindPolicy, Event: EventStatus,
					Version: rev.Version, Hash: rev.Hash, Status: rev.Status, Error: rev.LoadError,
				})
			}
			continue
		}
		r.add(r.revision(ws, name, rev, drafts))
		st.Policy = rev.Version
		if rev.Status == "pending" {
			st.Pending = append(st.Pending, rev.Version)
		}
	}
	return nil
}

func parseRevision(rev osclient.Revision) (*policy.Doc, error) {
	if rev.Err != "" {
		return nil, errors.New(rev.Err)
	}
	return policy.Parse([]byte(rev.Policy))
}

// revision is the record of a new revision of the sandbox's policy, which
// it diffs against the one before and attributes.
func (r *collectorRun) revision(ws, name string, rev osclient.Revision, drafts []store.Draft) Record {
	key := ws + "/" + name
	rec := Record{
		T: cmp.Or(rev.Created, time.Now()), Workspace: ws, Sandbox: name, Kind: KindPolicy, Event: EventRevision,
		Version: rev.Version, Hash: rev.Hash, Status: rev.Status, Error: rev.LoadError, Annotations: rev.Provenance,
	}
	doc, err := parseRevision(rev)
	if err != nil {
		rec.Error = strings.TrimPrefix(rec.Error+"; ", "; ") + "cannot read the policy: " + err.Error()
	} else {
		prev := r.docs[key]
		if prev == nil {
			prev = &policy.Doc{}
		}
		rec.Rules = map[string]policy.Rule{}
		for _, d := range policy.Diff(doc, prev).Rules {
			switch d.State {
			case policy.Unsaved:
				rec.Added = append(rec.Added, d.Key)
				rec.Rules[d.Key] = d.Sandbox
			case policy.NotApplied:
				rec.Removed = append(rec.Removed, d.Key)
			case policy.Changed:
				rec.Changed = append(rec.Changed, d.Key)
				rec.Rules[d.Key] = d.Sandbox
			}
		}
		if len(rec.Rules) == 0 {
			rec.Rules = nil
		}
		r.docs[key] = doc
	}
	by := rev.Provenance[policy.AnnotationBy]
	switch {
	case by == policy.ByPolicyAllow:
		rec.By, rec.Trace = ByPolicyAllow, rev.Provenance[policy.AnnotationTrace]
	case by != "":
		rec.By, rec.Trace = "brig "+strings.ReplaceAll(by, "-", " "), rev.Provenance[policy.AnnotationTrace]
	case rev.Version == 1:
		rec.By = BySandboxCreate
	default:
		if d, ok := approvedDraft(rec, drafts); ok {
			rec.By, rec.Chunk = "draft "+d.ID+" approved", d.ID
		} else {
			rec.By = ByUnknown
		}
	}
	return rec
}

// approvedDraft finds the draft whose approval made the revision: one
// approved when the revision was made, with a rule the revision added or
// changed.
func approvedDraft(rec Record, drafts []store.Draft) (store.Draft, bool) {
	var best store.Draft
	found := false
	for _, d := range drafts {
		made := slices.Contains(rec.Added, d.Rule) || slices.Contains(rec.Changed, d.Rule)
		if d.Status != "approved" || d.Decided.IsZero() || !made {
			continue
		}
		dist := d.Decided.Sub(rec.T).Abs()
		if dist > draftMatch {
			continue
		}
		if !found || dist < best.Decided.Sub(rec.T).Abs() {
			best, found = d, true
		}
	}
	return best, found
}

// actions records the brig commands that worked on the VM and ended since
// the last ones recorded.
func (r *collectorRun) actions() {
	if r.tail == nil || !r.tail.Poll() {
		return
	}
	runs := r.red.Runs()
	slices.Reverse(runs)
	until := r.pos.Until
	for _, ru := range runs {
		if ru.VM != r.VM || !ru.Done() || ru.End.Before(r.pos.Until) || slices.Contains(r.pos.Actions, ru.ID) {
			continue
		}
		r.add(Record{
			T: *ru.End, Kind: KindAction, Command: ru.Title, Status: ru.Status, Error: ru.Err, Trace: ru.Trace,
		})
		r.pos.Actions = append(r.pos.Actions, ru.ID)
		if ru.End.After(until) {
			until = *ru.End
		}
	}
	r.pos.Until = until
	if n := len(r.pos.Actions); n > actionsKept {
		r.pos.Actions = slices.Clone(r.pos.Actions[n-actionsKept:])
	}
}

// drops records what fence 2 dropped since the spool was last read, and
// the state of its listener.
func (r *collectorRun) drops() {
	var recs []Record
	pos, lost, err := fence2.ReadSpool(r.Store.SpoolPath(r.VM), r.pos.Spool, func(e fence2.Entry) {
		if e.State == "" {
			recs = append(recs, Record{
				T: e.T, Last: e.Last, Kind: KindDrop, Reason: e.Reason, Proto: e.Proto,
				Host: e.Addr, Port: e.Port, Hits: e.Hits, Why: e.Why,
			})
			return
		}
		r.w.Update(func(s *CollectorState) { s.Fence2, s.Fence2Why = e.State, e.Why })
		switch e.State {
		case fence2.CountsOnly:
			recs = append(recs, Record{T: e.T, Kind: KindGap, Why: "Fence 2 counts drops only, without their addresses: " + e.Why})
		case fence2.Off:
			recs = append(recs, Record{T: e.T, Kind: KindGap, Why: "Fence 2 records no drops: " + e.Why})
		}
	})
	if lost {
		recs = append([]Record{{Kind: KindGap, Why: "The spool of fence 2's drops was rotated twice before the audit collector read it; drops from then are missing."}}, recs...)
	}
	if err != nil {
		r.log().Warn("cannot read the drops of fence 2", "err", err)
	}
	r.pos.Spool = pos
	r.add(recs...)
}

// Drain records what is left in the VM's spool of fence 2's drops and
// returns, for after the VM's network stopped: its collector stops first,
// so brig stop drains the spool, lest the last drops wait for the next
// start or, after brig delete, for ever. While a collector runs, it reads
// the spool itself, and Drain does nothing.
func (c *Collector) Drain() error {
	spool := c.Store.SpoolPath(c.VM)
	if !exists(spool) && !exists(spool+".1") {
		return nil
	}
	w, err := c.Store.OpenWriter(c.VM)
	if errors.Is(err, ErrCollecting) {
		return nil
	}
	if err != nil {
		return err
	}
	r := &collectorRun{Collector: c, w: w}
	if err := c.Store.ReadPositions(c.VM, &r.pos); err != nil {
		return errors.Join(err, w.Close(nil))
	}
	stopped := w.index.Collector.Stopped
	r.drops()
	// The collector stopped when it stopped; this was no collector run.
	w.Update(func(s *CollectorState) { s.Stopped = stopped })
	return w.release(r.pos)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// retain deletes the days older than the retention, once a day.
func (r *collectorRun) retain(now time.Time) {
	today := dayOf(now)
	if r.Retention <= 0 || r.pos.Retained == today {
		return
	}
	r.pos.Retained = today
	plan, err := r.Store.PlanPrune(PruneSpec{VMs: []string{r.VM}, Before: now.Add(-r.Retention)}, nil)
	if err == nil && len(plan.Items) > 0 {
		err = r.Store.Prune(plan)
		r.log().Info("deleted audit days past the retention", "vm", r.VM, "days", len(plan.Items), "bytes", plan.Disk)
	}
	if err != nil {
		r.log().Error("cannot apply the audit retention", "err", err)
	}
}

// positions returns where the collector resumes.
func (r *collectorRun) positions() positions {
	p := r.pos
	p.Logs = map[string]ocwatch.Position{}
	for k, pos := range r.watcher.Positions() {
		p.Logs[k.String()] = pos
	}
	return p
}
