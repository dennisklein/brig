// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var day1 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// writeDays writes records of VM dev on three days, as a collector would:
// each day's requests of webapp and api, and a policy change of webapp on
// the second.
func writeDays(t *testing.T, s Store) {
	t.Helper()
	w, err := s.OpenWriter("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer w.unlock()
	for i := range 3 {
		now := day1.AddDate(0, 0, i)
		w.Now = func() time.Time { return now }
		w.Add(
			Record{T: now, Workspace: "default", Sandbox: "webapp", Kind: KindRequest, Verdict: VerdictAllow, Host: "pypi.org", Port: 443, Binary: "/usr/bin/pip", Rule: "pypi"},
			Record{T: now.Add(time.Minute), Workspace: "default", Sandbox: "webapp", Kind: KindRequest, Verdict: VerdictDeny, Host: "api.github.com", Port: 443, Binary: "/usr/bin/curl"},
			// Read late: it happened before the day's first record.
			Record{T: now.Add(-time.Hour), Workspace: "team", Sandbox: "api", Kind: KindRequest, Verdict: VerdictAllow, Host: "example.com", Port: 80},
		)
		if i == 1 {
			w.Add(Record{T: now.Add(2 * time.Minute), Workspace: "default", Sandbox: "webapp", Kind: KindPolicy, Event: EventRevision, Version: 2, Added: []string{"github", "pypi"}, By: ByPolicyAllow})
		}
		if err := w.Flush(nil); err != nil {
			t.Fatal(err)
		}
	}
}

func readAll(t *testing.T, s Store, q Query) []Record {
	t.Helper()
	var out []Record
	if err := s.Read(q, func(r Record) bool { out = append(out, r); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriteAndRead(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeDays(t, s)
	for _, name := range []string{"2026-10-11.jsonl.gz", "2026-10-12.jsonl.gz", "2026-10-13.jsonl", "index.json"} {
		if _, err := os.Stat(filepath.Join(s.Dir("dev"), name)); err != nil {
			t.Error(err)
		}
	}
	x, err := s.ReadIndex("dev")
	if err != nil {
		t.Fatal(err)
	}
	d := x.Days["2026-10-12"]
	if d.File != "2026-10-12.jsonl.gz" || d.Rows() != 4 || d.Counts["default/webapp"][KindRequest].Rows != 2 ||
		d.Counts["default/webapp"][KindPolicy].Rows != 1 || !d.First.Equal(day1.AddDate(0, 0, 1).Add(-time.Hour)) || d.Disk == 0 || d.Bytes <= d.Disk {
		t.Errorf("index of the second day: %+v", d)
	}
	if !x.Collector.LastEvent.Equal(day1.AddDate(0, 0, 2).Add(time.Minute)) {
		t.Errorf("last event %v", x.Collector.LastEvent)
	}

	all := readAll(t, s, Query{})
	if len(all) != 10 {
		t.Fatalf("read %d records", len(all))
	}
	// Each day's records come in time order.
	if all[0].Sandbox != "api" || all[1].Host != "pypi.org" {
		t.Errorf("order: %+v", all[:2])
	}
	for _, c := range []struct {
		name string
		q    Query
		want int
	}{
		{"one sandbox", Query{Workspace: "default", Sandbox: "webapp"}, 7},
		{"denied", Query{Denied: true}, 3},
		{"denied and policy", Query{Denied: true, Kinds: []string{KindRequest, KindPolicy}}, 4},
		{"host and subdomains", Query{Host: "github.com"}, 3},
		{"host pattern", Query{Host: "*.org"}, 3},
		{"since", Query{Since: day1.AddDate(0, 0, 1)}, 6},
		{"range", Query{Since: day1.AddDate(0, 0, 1).Add(-2 * time.Hour), Until: day1.AddDate(0, 0, 2).Add(-2 * time.Hour)}, 4},
		{"other VM", Query{VMs: []string{"ci"}}, 0},
	} {
		if got := readAll(t, s, c.q); len(got) != c.want {
			t.Errorf("%s: %d records, want %d", c.name, len(got), c.want)
		}
	}
	if oldest, _ := s.Oldest([]string{"dev"}); !oldest.Equal(day1.Add(-time.Hour)) {
		t.Errorf("oldest %v", oldest)
	}
}

func TestFollow(t *testing.T) {
	FollowEvery = 5 * time.Millisecond
	s := Store{Root: t.TempDir()}
	w, err := s.OpenWriter("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer w.unlock()
	w.Add(Record{Kind: KindRequest, Verdict: VerdictAllow, Host: "old.example"})
	if err := w.Flush(nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan Record, 4)
	done := make(chan error, 1)
	go func() { done <- s.Follow(ctx, Query{Denied: true}, func(r Record) { got <- r }) }()
	time.Sleep(20 * time.Millisecond)
	w.Add(Record{Kind: KindRequest, Verdict: VerdictAllow, Host: "a.example"}, Record{Kind: KindRequest, Verdict: VerdictDeny, Host: "b.example"})
	if err := w.Flush(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.Host != "b.example" {
			t.Errorf("followed %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing followed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("more followed: %+v", <-got)
	}
}

func TestPrune(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeDays(t, s)
	exists := func(vm string) bool { return vm == "dev" }

	plan, err := s.PlanPrune(PruneSpec{Before: day1.AddDate(0, 0, 1)}, exists)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Day != "2026-10-11" || plan.Rows != 3 || plan.Disk == 0 {
		t.Fatalf("plan: %+v", plan)
	}
	if err := s.Prune(plan); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, Query{}); len(got) != 7 {
		t.Errorf("%d records after pruning a day", len(got))
	}

	plan, err = s.PlanPrune(PruneSpec{Workspace: "team", Sandbox: "api"}, exists)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 || plan.Rows != 2 {
		t.Fatalf("sandbox plan: %+v", plan)
	}
	if err := s.Prune(plan); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, Query{}); len(got) != 5 || strings.Contains(recordsText(got), "example.com") {
		t.Errorf("records after pruning a sandbox: %+v", got)
	}
	x, _ := s.ReadIndex("dev")
	if _, ok := x.Sandboxes["team/api"]; ok || x.Days["2026-10-12"].Counts["team/api"] != nil || x.Days["2026-10-12"].Rows() != 3 {
		t.Errorf("index after pruning a sandbox: %+v", x)
	}

	// A deleted VM's history goes as a whole.
	if found, err := s.MarkDeleted("dev", time.Now()); !found || err != nil {
		t.Fatal(found, err)
	}
	plan, _ = s.PlanPrune(PruneSpec{DeletedVMs: true}, exists)
	if len(plan.Items) != 0 {
		t.Errorf("pruned an existing VM: %+v", plan)
	}
	plan, _ = s.PlanPrune(PruneSpec{DeletedVMs: true}, func(string) bool { return false })
	if len(plan.Items) != 1 || plan.Items[0].Day != "" || plan.Rows != 5 {
		t.Fatalf("deleted VMs plan: %+v", plan)
	}
	if err := s.Prune(plan); err != nil {
		t.Fatal(err)
	}
	if vms, _ := s.VMs(); len(vms) != 0 {
		t.Errorf("VMs left: %v", vms)
	}
}

func recordsText(rs []Record) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(Describe(r) + "\n")
	}
	return b.String()
}

func TestDescribe(t *testing.T) {
	for _, c := range []struct {
		r    Record
		want string
	}{
		{
			Record{Kind: KindRequest, Verdict: VerdictDeny, Method: "GET", Host: "api.github.com", Port: 443, Path: "/repos", Binary: "/usr/bin/curl", PID: 7, Reason: "no rule"},
			"deny GET api.github.com:443/repos by /usr/bin/curl (pid 7): no rule",
		},
		{Record{Kind: KindRequest, Verdict: VerdictAllow, Host: "pypi.org", Port: 443, Rule: "pypi"}, "allow pypi.org:443, rule pypi"},
		{
			Record{Kind: KindPolicy, Event: EventRevision, Version: 4, Status: "loaded", Added: []string{"a"}, Removed: []string{"b"}, By: ByPolicyAllow},
			"policy v4 (loaded) +a -b; by brig policy allow",
		},
		{Record{Kind: KindPolicy, Event: EventStatus, Version: 4, Status: "failed", Error: "bad"}, "policy v4 failed: bad"},
		{
			Record{Kind: KindDraft, Chunk: "c1", Status: "pending", Rule: "npm", Endpoints: []string{"registry.npmjs.org:443"}, Binary: "/usr/bin/npm", Hits: 2},
			"draft c1 pending: rule npm for registry.npmjs.org:443 by /usr/bin/npm (2 hits)",
		},
		{Record{Kind: KindSandbox, Event: EventCreated, Image: "agent:1"}, "sandbox created from agent:1"},
		{Record{Kind: KindAction, Command: "brig stop dev", Status: "failed", Error: "timeout"}, "brig stop dev (failed: timeout)"},
	} {
		if got := Describe(c.r); got != c.want {
			t.Errorf("Describe = %q, want %q", got, c.want)
		}
	}
}

func TestJournal(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "journal.sock")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	j, err := OpenJournal(sock, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	allowed := Record{VM: "dev", Workspace: "default", Sandbox: "webapp", Kind: KindRequest, Verdict: VerdictAllow, Host: "pypi.org"}
	denied := allowed
	denied.Verdict, denied.Host, denied.Reason = VerdictDeny, "evil.example", "line one\nline two"
	for _, r := range []Record{allowed, denied} {
		if err := j.Send(r); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 64<<10)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	entry := buf[:n]
	for _, want := range []string{"PRIORITY=4\n", "BRIG_VM=dev\n", "BRIG_SANDBOX=webapp\n", "BRIG_HOST=evil.example\n", "SYSLOG_IDENTIFIER=brig-audit\n"} {
		if !bytes.Contains(entry, []byte(want)) {
			t.Errorf("entry lacks %q:\n%s", want, entry)
		}
	}
	// A value with a newline is sent with its length.
	msg := "dev default/webapp: deny evil.example: line one\nline two"
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(msg)))
	if !bytes.Contains(entry, append(append([]byte("MESSAGE\n"), size[:]...), msg+"\n"...)) {
		t.Errorf("message not encoded:\n%q", entry)
	}
	// The allowed request was not mirrored: only one entry came.
	_ = l.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, err := l.Read(buf); err == nil {
		t.Error("allowed request mirrored")
	}
}

func TestSummary(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeDays(t, s)
	got, err := s.Summary(Query{Workspace: "default", Sandbox: "webapp", Since: day1.AddDate(0, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Endpoint != "api.github.com:443" || got[0].Denied != 1 || got[1].Endpoint != "pypi.org:443" ||
		got[1].Allowed != 1 || got[1].Rules[0] != "pypi" || got[1].Via["pypi"] != ByPolicyAllow || got[1].Binaries[0] != "/usr/bin/pip" {
		t.Errorf("summary: %+v", got)
	}
}

func TestRecordsAndSkippedDays(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeDays(t, s)
	// Only the second day has a policy record, so only its file is read.
	if err := os.Remove(filepath.Join(s.Dir("dev"), "2026-10-11.jsonl.gz")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir("dev"), "2026-10-13.jsonl"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := readAll(t, s, Query{Workspace: "default", Sandbox: "webapp", Kinds: []string{KindPolicy}})
	if len(pol) != 1 || pol[0].Version != 2 {
		t.Fatalf("policy records: %+v", pol)
	}
	mem := Records{pol[0], {VM: "ci", T: day1, Kind: KindGap, Why: "x"}}
	var got []Record
	_ = mem.Read(Query{VMs: []string{"dev"}}, func(r Record) bool { got = append(got, r); return true })
	if len(got) != 1 || got[0].Kind != KindPolicy {
		t.Errorf("in-memory read: %+v", got)
	}
}
