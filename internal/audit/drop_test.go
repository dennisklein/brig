// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/fence2"
)

func spoolDrops(t *testing.T, s Store, es ...fence2.Entry) {
	t.Helper()
	if err := (fence2.Spool{Path: s.SpoolPath("dev")}).Append(es...); err != nil {
		t.Fatal(err)
	}
}

func TestDrain(t *testing.T) {
	s := Store{Root: t.TempDir()}
	// Without a spool, as for a VM never started, it leaves no trail.
	if err := (&Collector{VM: "dev", Store: s}).Drain(); err != nil {
		t.Fatal(err)
	}
	if vms, _ := s.VMs(); len(vms) != 0 {
		t.Fatalf("draining nothing made a trail: %v", vms)
	}
	t0 := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	spoolDrops(t, s,
		fence2.Entry{T: t0, State: fence2.CountsOnly, Why: "no nfnetlink_log"},
		fence2.Entry{T: t0.Add(time.Second), Last: t0.Add(2 * time.Second), Reason: fence2.ReasonLAN, Proto: "tcp", Addr: "192.168.1.20", Port: 445, Hits: 3},
		fence2.Entry{T: t0, Last: t0.Add(fence2.Window), Reason: fence2.ReasonUDPFlows, Hits: 18234, Why: fence2.WhyOverRate},
	)
	c := &Collector{VM: "dev", Store: s}

	// A running collector reads the spool itself.
	w, err := s.OpenWriter("dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(positions{}); err != nil {
		t.Fatal(err)
	}
	if recs := readRecords(t, s); len(recs) != 0 {
		t.Fatalf("drained while a collector ran: %+v", recs)
	}

	if err := c.Drain(); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, s)
	if len(recs) != 3 {
		t.Fatalf("records %+v", recs)
	}
	if gap := recs[0]; gap.Kind != KindGap || !strings.Contains(gap.Why, "counts drops only") || !strings.Contains(gap.Why, "no nfnetlink_log") {
		t.Errorf("gap %+v", gap)
	}
	lan := Record{
		V: Version, T: t0.Add(time.Second), Last: t0.Add(2 * time.Second), VM: "dev", Kind: KindDrop,
		Reason: fence2.ReasonLAN, Proto: "tcp", Host: "192.168.1.20", Port: 445, Hits: 3,
	}
	if !reflect.DeepEqual(recs[2], lan) {
		t.Errorf("drop\n%+v\nwant\n%+v", recs[2], lan)
	}
	if flood := recs[1]; flood.Reason != fence2.ReasonUDPFlows || flood.Hits != 18234 || flood.Why != fence2.WhyOverRate || flood.Host != "" {
		t.Errorf("flood %+v", flood)
	}
	if got := Describe(recs[2]); got != "drop tcp 192.168.1.20:445 ×3: lan is off" {
		t.Errorf("Describe() = %q", got)
	}
	if !recs[2].Change() {
		t.Error("the journal mirror leaves drops out")
	}
	for _, want := range []string{"MESSAGE=dev: drop tcp 192.168.1.20:445 ×3: lan is off\n", "PRIORITY=4\n", "BRIG_KIND=drop\n", "BRIG_REASON=lan\n", "BRIG_PORT=445\n"} {
		if entry := string(JournalEntry(recs[2])); !strings.Contains(entry, want) {
			t.Errorf("journal entry lacks %q:\n%s", want, entry)
		}
	}
	if got := Describe(recs[1]); got != "drop ×18234: too many UDP flows at once (over the logging rate, not itemised)" {
		t.Errorf("Describe() = %q", got)
	}
	st, err := s.Status(context.Background(), []string{"dev"}, func(string) VMInfo { return VMInfo{Exists: true, Known: true} })
	if err != nil {
		t.Fatal(err)
	}
	if st[0].Fence2 != fence2.CountsOnly || st[0].Fence2Why != "no nfnetlink_log" || st[0].Collector != StateVMStopped {
		t.Errorf("status %+v", st[0])
	}

	// Drained again, nothing is recorded twice; what came since is.
	spoolDrops(t, s, fence2.Entry{T: t0.Add(10 * time.Second), Reason: fence2.ReasonHost, Proto: "udp", Addr: "10.0.2.2", Port: 53, Hits: 1})
	if err := c.Drain(); err != nil {
		t.Fatal(err)
	}
	if recs := readRecords(t, s); len(recs) != 4 || recs[3].Reason != fence2.ReasonHost {
		t.Errorf("after a second drain: %+v", recs)
	}
}

func TestCollectorReadsDrops(t *testing.T) {
	s := Store{Root: t.TempDir()}
	spoolDrops(t, s,
		fence2.Entry{T: time.Now(), State: fence2.Itemised},
		fence2.Entry{T: time.Now(), Reason: fence2.ReasonHostPort, Proto: "tcp", Addr: "10.0.2.2", Port: 8080, Hits: 1},
	)
	recs := collect(t, s, newFakeGateway(), "", func(recs []Record) bool { return len(kinds(recs, KindDrop)) == 1 })
	if d := kinds(recs, KindDrop)[0]; d.Port != 8080 || d.Sandbox != "" || len(kinds(recs, KindGap)) != 0 {
		t.Errorf("records %+v", recs)
	}
	x, err := s.ReadIndex("dev")
	if err != nil {
		t.Fatal(err)
	}
	if x.Collector.Fence2 != fence2.Itemised {
		t.Errorf("fence 2 state %q", x.Collector.Fence2)
	}
}

func readRecords(t *testing.T, s Reader) []Record {
	t.Helper()
	var recs []Record
	if err := s.Read(Query{}, func(r Record) bool { recs = append(recs, r); return true }); err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestQueryDrops(t *testing.T) {
	drop := Record{Kind: KindDrop, Host: "192.168.1.20", Port: 445, Reason: fence2.ReasonLAN}
	allowed := Record{Kind: KindRequest, Verdict: VerdictAllow, Host: "nas.lan", Port: 445}
	for _, tc := range []struct {
		q    Query
		r    Record
		want bool
	}{
		{Query{Denied: true}, drop, true},
		{Query{Denied: true}, allowed, false},
		{Query{Denied: true, Kinds: []string{KindPolicy}}, drop, false},
		{Query{Kinds: []string{KindDrop}}, drop, true},
		{Query{Sandbox: "webapp"}, drop, false},
		{Query{Host: "192.168.1.20"}, drop, true},
		{Query{Host: "1.20"}, drop, false},
		{Query{Host: "192.168.0.0/16"}, drop, true},
		{Query{Host: "10.0.0.0/8"}, drop, false},
		{Query{Host: "192.168.1.*"}, drop, true},
		{Query{Host: "nas.lan"}, drop, false},
		{Query{Host: "fd00::1"}, Record{Kind: KindDrop, Host: "fd00::1"}, true},
	} {
		if got := tc.q.Match(tc.r); got != tc.want {
			t.Errorf("%+v.Match(%+v) = %v", tc.q, tc.r, got)
		}
	}
}

func TestHintsAndSummary(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 9, 12, 3, 0, time.UTC)
	req := func(at time.Duration, sandbox, host string, port int, verdict string) Record {
		return Record{V: Version, T: t0.Add(at), VM: "dev", Workspace: "default", Sandbox: sandbox, Kind: KindRequest, Verdict: verdict, Host: host, Port: port}
	}
	drop := func(at, last time.Duration, host string, port, hits int) Record {
		r := Record{V: Version, T: t0.Add(at), VM: "dev", Kind: KindDrop, Reason: fence2.ReasonLAN, Proto: "tcp", Host: host, Port: port, Hits: hits}
		if last > at {
			r.Last = t0.Add(last)
		}
		return r
	}
	recs := Records{
		req(-time.Second, "webapp", "nas.lan", 445, VerdictAllow),
		drop(0, 2*time.Second, "192.168.1.20", 445, 3),
		req(10*time.Second, "webapp", "nas.lan", 445, VerdictAllow),
		req(10*time.Second, "api", "printer.lan", 445, VerdictAllow),
		drop(11*time.Second, 0, "192.168.1.20", 445, 1),
		req(20*time.Second, "webapp", "nas.lan", 445, VerdictDeny),
		drop(21*time.Second, 0, "192.168.1.20", 445, 1),
		req(28*time.Second, "webapp", "pypi.org", 443, VerdictAllow),
		drop(31*time.Second, 0, "10.1.1.1", 443, 2),
		{V: Version, T: t0.Add(40 * time.Second), VM: "dev", Kind: KindDrop, Reason: fence2.ReasonUDPFlows, Hits: 900, Why: fence2.WhyOverRate},
	}
	hints, err := Hints(recs, Query{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		drop Record
		want Hint
	}{
		{recs[1], Hint{Sandboxes: []string{"default/webapp"}, Host: "nas.lan", Port: 445, At: t0.Add(-time.Second)}},
		{recs[4], Hint{Sandboxes: []string{"default/api", "default/webapp"}, Host: "printer.lan", Port: 445, At: t0.Add(10 * time.Second)}},
		// A denied request does not explain a drop.
		{recs[6], Hint{}},
		// Too long before.
		{recs[8], Hint{}},
	} {
		if got, ok := hints[DropKey(tc.drop)]; !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("hint of %s = %+v, %v; want %+v", Describe(tc.drop), got, ok, tc.want)
		}
	}
	if _, ok := hints[DropKey(recs[9])]; ok {
		t.Error("a drop that was counted only has a hint")
	}

	reach, err := Summarize(recs, Query{})
	if err != nil {
		t.Fatal(err)
	}
	var drops []Reach
	for _, x := range reach {
		if x.Dropped > 0 {
			drops = append(drops, x)
		}
	}
	if len(drops) != 3 || drops[0].Reason != fence2.ReasonUDPFlows || drops[0].Endpoint != "" ||
		drops[1].Endpoint != "192.168.1.20:445" || drops[1].Dropped != 5 || drops[1].FromVM != 1 ||
		!reflect.DeepEqual(drops[1].Likely, []string{"default/api", "default/webapp"}) || !drops[1].Last.Equal(t0.Add(21*time.Second)) {
		t.Errorf("drops summed up: %+v", drops)
	}
	if reach, _ := Summarize(recs, Query{Sandbox: "webapp", Workspace: "default"}); len(reach) != 2 {
		t.Errorf("a sandbox's summary: %+v", reach)
	}
	if reach, _ := Summarize(recs, Query{Kinds: []string{KindRequest}}); len(reach) != 3 {
		t.Errorf("a summary of requests: %+v", reach)
	}

	// Following, a hinter sees the requests first.
	h := &Hinter{}
	for _, r := range recs[:4] {
		h.Observe(r)
	}
	h.Forget(t0.Add(5 * time.Second))
	if got, _ := h.Hint(recs[1]); len(got.Sandboxes) != 0 {
		t.Errorf("forgotten requests hinted: %+v", got)
	}
	if got, _ := h.Hint(recs[4]); len(got.Sandboxes) != 2 {
		t.Errorf("hint %+v", got)
	}
}
